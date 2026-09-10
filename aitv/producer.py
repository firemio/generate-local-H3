"""Producer loop: schedule -> LLM script -> H3 render -> library/playlist.

Designed to run unattended for days: every step that can fail (LLM, ComfyUI, ffmpeg, index write)
is contained, and a wedged or dead ComfyUI is escalated (interrupt -> free -> restart) instead of
stalling the channel forever.
"""
from __future__ import annotations

import os
import random
import re
import shutil
import subprocess
import sys
import time
import uuid

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

from h3gen.client import ComfyClient  # noqa: E402
from h3gen.graph import GenSpec, build  # noqa: E402
from . import index_store  # noqa: E402
from .llm import LLM  # noqa: E402
from .programs import FORMATS, fallback_programme, pick_format, writer_prompt  # noqa: E402

LIB = index_store.LIB
RAW_DIR = os.path.join(ROOT, "output", "aitv")  # ComfyUI SaveVideo target for aitv/ prefixed jobs

load_index = index_store.load
save_index = index_store.save

LANG_TAG = "[Japanese]"


def sanitize_prompt(prompt: str, lang_tag: str = LANG_TAG) -> str:
    """Make LLM output safe for H3: every <d>...</d> gets the language tag, [ja]-style tags are expanded,
    stray markdown fences are removed."""
    prompt = re.sub(r"^```(?:\w+)?\s*|\s*```$", "", prompt.strip(), flags=re.S)
    prompt = re.sub(r"<d>\s*\[(ja|jp|japanese|日本語)\]\s*", f"<d>{lang_tag} ", prompt, flags=re.I)
    prompt = re.sub(r"<d>(?!\s*\[)\s*", f"<d>{lang_tag} ", prompt)
    return prompt


def normalize(src: str, dst: str, width: int, height: int) -> None:
    """Re-encode to a uniform h264/aac mp4 (same size, 24 fps, 48 kHz stereo) so the broadcaster can stream-copy."""
    cmd = ["ffmpeg", "-y", "-hide_banner", "-loglevel", "error", "-i", src,
           "-vf", f"scale={width}:{height}:flags=lanczos,format=yuv420p", "-r", "24",
           "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-profile:v", "high", "-level", "4.1",
           "-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
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
    def __init__(self, comfy: ComfyClient, llm: LLM | None, width: int = 832, height: int = 480,
                 steps: int = 8, seed: int | None = None, log=print, restart_comfy=None):
        self.comfy, self.llm = comfy, llm
        self.width, self.height, self.steps = width, height, steps
        self.rng = random.Random(seed)
        self.log = log
        self.last_format = None
        self.restart_comfy = restart_comfy  # optional callable to relaunch ComfyUI

    # ---- script ------------------------------------------------------------
    def write_programme(self, fmt_key: str) -> dict:
        if self.llm is not None:
            try:
                system, user = writer_prompt(fmt_key)
                prog = self.llm.chat_json(system, user)
                segs = [s for s in prog.get("segments", []) if isinstance(s, dict) and s.get("prompt")]
                if segs:
                    prog["segments"] = segs
                    prog.setdefault("title", FORMATS[fmt_key]["title"])
                    return prog
                self.log("[aitv] LLM returned no segments, using fallback")
            except Exception as e:
                self.log(f"[aitv] LLM failed ({e}); using fallback script")
        return fallback_programme(fmt_key)

    # ---- render ------------------------------------------------------------
    def render_segment(self, fmt_key: str, prog_title: str, seg: dict) -> dict | None:
        seconds = float(seg.get("seconds") or FORMATS[fmt_key]["seconds"])
        seconds = max(3.0, min(seconds, 10.0))
        seed = self.rng.randrange(1, 2**31)
        seg["prompt"] = sanitize_prompt(seg["prompt"])
        spec = GenSpec(prompt=seg["prompt"], mode="t2v", width=self.width, height=self.height, seconds=seconds,
                       seed=seed, steps=self.steps, turbo=True, filename_prefix="aitv/seg")
        res = self.comfy.run(build(spec), log=lambda *_: None, stall_timeout=1200)
        if not res.ok or not res.local_files:
            self.log(f"[aitv] render failed: {res.error}")
            return None
        uid = time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:6]
        dst = os.path.join(LIB, f"{fmt_key}-{uid}.mp4")
        os.makedirs(LIB, exist_ok=True)
        raw = res.local_files[0]
        try:
            normalize(raw, dst, self.width, self.height)
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
        item = {"id": uid, "file": os.path.basename(dst), "format": fmt_key, "program": prog_title,
                "caption": seg.get("caption") or prog_title, "seconds": spec.length / 24.0, "seed": seed,
                "render_s": round(res.seconds, 1), "created": time.strftime("%Y-%m-%dT%H:%M:%S"),
                "prompt": seg["prompt"]}
        self.log(f"[aitv] + {item['file']}  ({item['seconds']:.1f}s clip, rendered in {res.seconds:.0f}s, "
                 f"sampling {res.node_times.get('sample', 0):.0f}s) [{item['caption']}]")
        return item

    def produce_one(self, fmt_key: str | None = None) -> list[dict]:
        fmt_key = fmt_key or pick_format(self.rng, exclude=self.last_format)
        self.last_format = fmt_key
        prog = self.write_programme(fmt_key)
        title = prog.get("title", FORMATS[fmt_key]["title"])
        self.log(f"[aitv] === {FORMATS[fmt_key]['title']}: {title} ({len(prog['segments'])} seg)")
        made = []
        for seg in prog["segments"]:
            item = self.render_segment(fmt_key, title, seg)
            if item:
                made.append(item)
                try:
                    index_store.append(item)   # a failed index write must not lose the other segments
                except Exception as e:
                    self.log(f"[aitv] index write failed ({e}); clip kept on disk as {item['file']}")
        return made

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
        self.comfy.interrupt()
        self.comfy.free()
        if failures >= 4 and self.restart_comfy:
            self.log("[aitv] restarting ComfyUI")
            try:
                self.restart_comfy()
            except Exception as e:
                self.log(f"[aitv] restart failed: {e}")
            time.sleep(30)
            self.wait_for_comfy()

    # ---- loop --------------------------------------------------------------
    def run(self, count: int | None = None, formats: list[str] | None = None) -> None:
        """Production loop. count=None -> run forever (24/7 station); every failure is logged and
        retried with a growing pause so one bad programme never stops the channel."""
        n, failures = 0, 0
        while count is None or n < count:
            fmt = formats[n % len(formats)] if formats else None
            try:
                self.wait_for_comfy()
                made = self.produce_one(fmt)
                n += 1
                if made:
                    failures = 0
                    sweep_raw(log=self.log)
                else:
                    failures += 1
            except KeyboardInterrupt:
                raise
            except Exception as e:  # LLM/HTTP/ffmpeg/... keep the station alive
                failures += 1
                self.log(f"[aitv] programme failed ({type(e).__name__}: {e}); retrying")
            if failures:
                try:
                    self.recover_comfy(failures)
                except Exception as e:
                    self.log(f"[aitv] recovery failed: {e}")
                pause = min(300, 20 * failures)
                self.log(f"[aitv] pausing {pause}s")
                time.sleep(pause)
