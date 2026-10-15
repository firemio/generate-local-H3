"""Producer loop: lineup slot -> real material -> two-pass script -> H3 renders clip by clip -> library.

Designed to run unattended for days: every step that can fail (LLM, ComfyUI, ffmpeg, index write)
is contained, and a wedged or dead ComfyUI is escalated (interrupt -> free -> restart) instead of
stalling the channel forever. The next programme's script is written on the CPU while the current
one renders on the GPU.
"""
from __future__ import annotations

import json
import os
import random
import shutil
import subprocess
import sys
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

from h3gen.client import ComfyClient  # noqa: E402
from h3gen.graph import GenSpec, build  # noqa: E402
from . import index_store, script, sources  # noqa: E402
from .llm import LLM  # noqa: E402
from . import loudness  # noqa: E402
from .programs import BREAK_FORMATS, FORMATS, LINEUP  # noqa: E402

LIB = index_store.LIB
RAW_DIR = os.path.join(ROOT, "output", "aitv")  # ComfyUI SaveVideo target for aitv/ prefixed jobs
LIB_SIZE = (832, 480)  # every library clip is normalised to this so the HLS stream-copy never changes size

load_index = index_store.load
save_index = index_store.save


def normalize(src: str, dst: str, width: int, height: int) -> None:
    """Re-encode to a uniform h264/aac mp4 (library size, 24 fps, 48 kHz stereo, one loudness level)
    so the broadcaster can stream-copy and the channel does not jump 20 dB between clips (aitv/loudness.py)."""
    cmd = ["ffmpeg", "-y", "-hide_banner", "-loglevel", "error", "-i", src,
           "-vf", f"scale={width}:{height}:flags=lanczos,format=yuv420p", "-r", "24",
           "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-profile:v", "high", "-level", "4.1",
           "-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
           *loudness.audio_filter_args(src),
           "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2",
           "-movflags", "+faststart", dst]
    subprocess.run(cmd, check=True, timeout=900)


def sweep_raw(older_than_s: float = 3600, log=print) -> int:
    """Delete ComfyUI's raw renders once they are in the library (they are exact duplicates:
    ~1.4 MB per clip, ~200 clips/day). Keeps recent files in case a run is still in flight."""
    if not os.path.isdir(RAW_DIR):
        return 0
    now, n = time.time(), 0
    for name in os.listdir(RAW_DIR):
        p = os.path.join(RAW_DIR, name)
        try:
            if os.path.isfile(p) and now - os.path.getmtime(p) > older_than_s:
                os.remove(p)
                n += 1
        except OSError:
            pass
    if n:
        log(f"[aitv] swept {n} raw render(s) from output/aitv")
    return n


class Producer:
    def __init__(self, comfy: ComfyClient, llm: LLM, width: int = 640, height: int = 352, steps: int = 8,
                 seed: int | None = None, log=print, restart_comfy=None, turbo: bool = True,
                 lineup: list[str] | None = None, minutes: float | None = None):
        self.comfy, self.llm = comfy, llm
        self.width, self.height, self.steps, self.turbo = width, height, steps, turbo
        self.rng = random.Random(seed)
        self.log = log
        self.restart_comfy = restart_comfy  # optional callable to relaunch ComfyUI
        self.lineup = lineup or list(LINEUP)
        self.minutes = minutes              # None = each format's own length
        self.slot = self._resume_slot()
        self.break_toggle = 0
        self._prefetch: tuple[int, threading.Thread, list] | None = None

    # ---- lineup ------------------------------------------------------------
    def _resume_slot(self) -> int:
        """Continue after the last programme that reached the library (survives restarts)."""
        for it in reversed(index_store.load()):
            if it.get("slot") is not None and it.get("format") == self.lineup[it["slot"] % len(self.lineup)]:
                return (int(it["slot"]) + 1) % len(self.lineup)
        return 0

    def _minutes(self, fmt_key: str) -> float:
        return float(self.minutes or FORMATS[fmt_key]["minutes"])

    def _unfinished(self) -> dict | None:
        """The newest programme in the index that still has clips to render (a restart or crash cut it
        short), with the indices already done, so hours of GPU work are not thrown away."""
        items = index_store.load()
        done: dict[str, set] = {}
        for it in items:
            if it.get("programme_id"):
                done.setdefault(it["programme_id"], set()).add(int(it.get("segment_index") or 0))
        for it in reversed(items):
            pid = it.get("programme_id")
            if not pid:
                continue
            total = int(it.get("segments_total") or 0)
            if not total or len(done[pid]) >= total:
                return None
            try:
                with open(os.path.join(script.SCRIPTS, pid + ".json"), encoding="utf-8") as fh:
                    prog = json.load(fh)
            except (OSError, ValueError):
                return None
            prog["_done"] = done[pid]
            return prog
        return None

    # ---- script ------------------------------------------------------------
    def _write(self, slot: int) -> dict | None:
        """Material + script for one slot; None when the slot cannot be made right now."""
        fmt_key = self.lineup[slot]
        gap_h = FORMATS[fmt_key].get("min_gap_hours")
        if gap_h:
            last = script.history(fmt_key, 1)
            if last:
                age_h = (time.time() - time.mktime(time.strptime(last[0]["written"], "%Y-%m-%dT%H:%M:%S"))) / 3600
                if age_h < gap_h:
                    self.log(f"[aitv] last {fmt_key} was written {age_h:.1f} h ago (< {gap_h} h): the material would be the same; "
                             f"slot {slot + 1} skipped this round")
                    return None
        material = sources.material_for(fmt_key, self.rng)
        if material is None and FORMATS[fmt_key].get("needs_material"):
            self.log(f"[aitv] no real material for {fmt_key} (offline?); slot {slot + 1} skipped this round")
            return None
        return script.write_programme(self.llm, fmt_key, material, self._minutes(fmt_key), slot=slot, log=self.log,
                                      rng=self.rng)

    def _start_prefetch(self, slot: int) -> None:
        box: list = []

        def run():
            try:
                box.append(self._write(slot))
            except Exception as e:  # noqa
                self.log(f"[aitv] writing slot {slot + 1} failed ({type(e).__name__}: {e})")
                box.append(None)
        t = threading.Thread(target=run, daemon=True)
        t.start()
        self._prefetch = (slot, t, box)

    def _script_for(self, slot: int) -> dict | None:
        if self._prefetch and self._prefetch[0] == slot:
            _, t, box = self._prefetch
            self._prefetch = None
            t.join()
            return box[0] if box else None
        self._prefetch = None
        return self._write(slot)

    # ---- render ------------------------------------------------------------
    def render_segment(self, prog: dict, idx: int, section: str, seg: dict) -> dict | None:
        seconds = float(seg["seconds"])
        seed = self.rng.randrange(1, 2**31)
        spec = GenSpec(prompt=seg["prompt"], mode="t2v", width=self.width, height=self.height, seconds=seconds,
                       seed=seed, steps=self.steps, turbo=self.turbo, filename_prefix="aitv/seg")
        res = self.comfy.run(build(spec), log=lambda *_: None, stall_timeout=1200)
        if not res.ok or not res.local_files:
            self.log(f"[aitv] render failed: {res.error}")
            return None
        uid = time.strftime("%Y%m%d-%H%M%S") + "-" + prog["id"][-6:] + f"-{idx:02d}"
        dst = os.path.join(LIB, f"{prog['format']}-{uid}.mp4")
        os.makedirs(LIB, exist_ok=True)
        raw = res.local_files[0]
        try:
            normalize(raw, dst, *LIB_SIZE)
        except Exception as e:
            self.log(f"[aitv] ffmpeg normalize failed ({e}); copying raw")
            try:
                shutil.copy(raw, dst)
            except OSError as e2:
                self.log(f"[aitv] could not keep the clip: {e2}")
                return None
        for p in res.local_files:  # the library copy is authoritative; drop the duplicate
            try:
                os.remove(p)
            except OSError:
                pass
        item = {"id": uid, "file": os.path.basename(dst), "format": prog["format"], "program": prog["title"],
                "programme_id": prog["id"], "slot": prog.get("slot"), "section": section, "segment_index": idx,
                "segments_total": prog["segments_total"], "caption": seg["caption"], "seconds": spec.length / 24.0,
                "seed": seed, "render_s": round(res.seconds, 1), "created": time.strftime("%Y-%m-%dT%H:%M:%S"),
                "prompt": seg["prompt"]}
        if prog.get("sources"):
            item["sources"] = prog["sources"][:8]
        self.log(f"[aitv] + {item['file']}  ({item['seconds']:.1f}s, rendered in {res.seconds:.0f}s) "
                 f"[{prog['title']} / {section} {idx + 1}/{prog['segments_total']}: {seg['caption']}]")
        return item

    def render_programme(self, prog: dict) -> tuple[int, int]:
        made = failed = 0
        skip = prog.get("_done") or set()
        for idx, section, seg in script.iter_segments(prog):
            if idx in skip:
                continue
            self.wait_for_comfy()
            item = self.render_segment(prog, idx, section, seg)
            if item:
                made += 1
                try:
                    index_store.append(item)   # a failed index write must not lose the other segments
                except Exception as e:
                    self.log(f"[aitv] index write failed ({e}); clip kept on disk as {item['file']}")
            else:
                failed += 1
                self.recover_comfy(failed)
                time.sleep(min(300, 20 * failed))
                if failed >= 6:
                    self.log(f"[aitv] giving up on {prog['title']} after {failed} failed renders")
                    break
        return made, failed

    # ---- health ------------------------------------------------------------
    def wait_for_comfy(self, poll: float = 15.0, restart_after: float = 600.0) -> None:
        """Block until the ComfyUI API answers. If it stays down past restart_after and a restart
        hook was given, relaunch it (the station must not depend on a human being at the keyboard)."""
        if self.comfy.alive():
            return
        self.log(f"[aitv] ComfyUI at {self.comfy.base} not reachable; waiting...")
        t0 = time.time()
        restarted = False
        while not self.comfy.alive():
            if not restarted and self.restart_comfy and time.time() - t0 > restart_after:
                self.log("[aitv] ComfyUI still down; relaunching it")
                try:
                    self.restart_comfy()
                except Exception as e:
                    self.log(f"[aitv] relaunch failed: {e}")
                restarted = True
                t0 = time.time()
            time.sleep(poll)
        self.log("[aitv] ComfyUI is back")

    def recover_comfy(self, failures: int) -> None:
        """Escalate after repeated render failures: /system_stats keeps answering after a HIP hang or
        a driver reset, so 'alive' is not enough evidence that the GPU still works."""
        self.log(f"[aitv] {failures} consecutive failure(s): interrupting and freeing models")
        try:
            self.comfy.interrupt()
            self.comfy.free()
        except Exception as e:
            self.log(f"[aitv] recovery call failed: {e}")
        if failures >= 4 and self.restart_comfy:
            self.log("[aitv] restarting ComfyUI")
            try:
                self.restart_comfy()
            except Exception as e:
                self.log(f"[aitv] restart failed: {e}")
            time.sleep(30)
            self.wait_for_comfy()

    def wait_for_llm(self) -> None:
        waited = False
        while not self.llm.alive():
            if not waited:
                self.log(f"[aitv] writer LLM at {self.llm.base_url} not answering; waiting...")
                waited = True
            time.sleep(30)
        if waited:
            self.log("[aitv] writer LLM is back")

    # ---- loop --------------------------------------------------------------
    def produce_break(self) -> None:
        """One fresh commercial or ident after each programme (cheap: 4-6 s of video)."""
        fmt_key = BREAK_FORMATS[self.break_toggle % len(BREAK_FORMATS)]
        self.break_toggle += 1
        try:
            prog = script.write_break(self.llm, fmt_key)
        except Exception as e:
            self.log(f"[aitv] {fmt_key} script failed ({e}); skipped")
            return
        self.render_programme(prog)

    def run(self, count: int | None = None, formats: list[str] | None = None) -> None:
        """Production loop over the lineup. count=None -> forever (24/7 station). `formats` replaces the
        lineup for this run (e.g. a one-off test)."""
        if formats:
            self.lineup = formats
            self.slot = 0
        n = 0
        unfinished = self._unfinished() if not formats else None
        if unfinished:
            try:
                self.wait_for_comfy()
                left = unfinished["segments_total"] - len(unfinished["_done"])
                self.log(f"[aitv] === resuming: {unfinished['title']} ({left} clips left)")
                made, failed = self.render_programme(unfinished)
                self.log(f"[aitv] === done: {unfinished['title']} ({made} clips, {failed} failed)")
                if made:
                    sweep_raw(log=self.log)
            except KeyboardInterrupt:
                raise
            except Exception as e:  # noqa
                self.log(f"[aitv] resume failed ({type(e).__name__}: {e}); moving on")
        while count is None or n < count:
            slot = self.slot
            fmt_key = self.lineup[slot]
            try:
                self.wait_for_llm()
                self.wait_for_comfy()
                self.log(f"[aitv] === slot {slot + 1}/{len(self.lineup)}: {FORMATS[fmt_key]['title']} ({self._minutes(fmt_key):g} min)")
                prog = self._script_for(slot)
                self.slot = (slot + 1) % len(self.lineup)
                n += 1
                if prog is None:
                    time.sleep(60)
                    continue
                if count is None or n < count:
                    self._start_prefetch(self.slot)      # write the next one while this one renders
                made, failed = self.render_programme(prog)
                self.log(f"[aitv] === done: {prog['title']} ({made} clips, {failed} failed)")
                if made:
                    sweep_raw(log=self.log)
                    self.produce_break()
            except KeyboardInterrupt:
                raise
            except Exception as e:  # LLM/HTTP/ffmpeg/... keep the station alive
                self.log(f"[aitv] programme failed ({type(e).__name__}: {e}); retrying in 60s")
                self.slot = (slot + 1) % len(self.lineup)
                time.sleep(60)
