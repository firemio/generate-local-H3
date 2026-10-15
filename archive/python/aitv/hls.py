"""Broadcaster: turns the library into one continuous live HLS stream with ffmpeg.

A single long-running ffmpeg reads MPEG-TS from stdin (-re = real-time pacing) and writes a sliding
HLS window to aitv/hls/live.m3u8. Python feeds it clip after clip (remuxed to TS, stream copy) in
the order the Rundown decides, block by block, and picks up new clips as the producer adds them.
Any HLS player (the web page via hls.js, VLC, OBS Media Source) can tune in; aitv/hls/now.json
tells the web page what is on air (keyed by wall-clock time, matched to the stream's
program-date-time on the player side).
"""
from __future__ import annotations

import json
import os
import subprocess
import time

from . import index_store
from .rundown import Rundown

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LIB = index_store.LIB
HLS = os.path.join(ROOT, "aitv", "hls")
NOW = os.path.join(HLS, "now.json")


def _log_on_air(on_air: list[dict], it: dict) -> None:
    on_air.append({"t": time.time(), "program": it.get("program", ""), "caption": it.get("caption", ""),
                   "section": it.get("section", ""), "format": it.get("format", ""), "seconds": it.get("seconds", 0),
                   "created": it.get("created", ""), "id": it.get("id", "")})
    on_air[:] = on_air[-40:]
    try:
        tmp = NOW + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump(on_air, fh, ensure_ascii=False)
        os.replace(tmp, NOW)
    except OSError:
        pass


def _start_encoder(segment_seconds: int, window: int) -> subprocess.Popen:
    # NOTE: no +igndts. The mpegts demuxer already has correct DTS; discarding it makes ffmpeg
    # reconstruct timestamps and clamp DTS to prev+1 tick across B-frame groups, which destroys
    # presentation timestamps after the first clip boundary (measured: 71% of frames affected).
    # +genpts alone is enough, and ffmpeg re-bases each clip's timestamps continuously.
    out = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-fflags", "+genpts",
           "-f", "mpegts", "-i", "pipe:0",
           "-c", "copy", "-f", "hls", "-hls_time", str(segment_seconds), "-hls_list_size", str(window),
           "-hls_flags", "delete_segments+append_list+omit_endlist+program_date_time",
           "-hls_segment_filename", os.path.join(HLS, "seg%06d.ts"), os.path.join(HLS, "live.m3u8")]
    return subprocess.Popen(out, stdin=subprocess.PIPE)


def _stop(proc: subprocess.Popen | None, close_stdin: bool = True) -> None:
    if proc is None:
        return
    try:
        if close_stdin and proc.stdin and not proc.stdin.closed:
            proc.stdin.close()
    except OSError:
        pass
    try:
        proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            pass


def broadcast(segment_seconds: int = 2, window: int = 10, log=print) -> None:
    os.makedirs(HLS, exist_ok=True)
    for f in os.listdir(HLS):
        try:
            os.remove(os.path.join(HLS, f))
        except OSError:
            pass
    proc = _start_encoder(segment_seconds, window)
    log("[aitv] HLS broadcaster started -> aitv/hls/live.m3u8")
    rundown = Rundown()
    queue: list[dict] = []
    on_air: list[dict] = []
    warned_empty = False
    try:
        while True:
            if proc.poll() is not None:  # encoder died: replace it and keep the channel going
                log(f"[aitv] HLS encoder exited ({proc.returncode}); restarting")
                _stop(proc)
                proc = _start_encoder(segment_seconds, window)
            if not queue:
                items = index_store.playable()  # only clips whose file is on disk
                queue = rundown.next_block(items) if items else []
                if not queue:
                    if not warned_empty:
                        log("[aitv] library empty, waiting for the producer...")
                        warned_empty = True
                    time.sleep(5)
                    continue
                warned_empty = False
                log("[aitv] block: " + " > ".join(f"{c.get('format')}" for c in queue))
            it = queue.pop(0)
            src = os.path.join(LIB, it["file"])
            if not os.path.exists(src):
                continue
            log(f"[aitv] >> {it['program']} / {it['caption']}")
            _log_on_air(on_air, it)
            remux = subprocess.Popen(["ffmpeg", "-hide_banner", "-loglevel", "error", "-i", src,
                                      "-c:v", "copy", "-bsf:v", "h264_mp4toannexb", "-c:a", "copy",
                                      "-f", "mpegts", "pipe:1"], stdout=subprocess.PIPE)
            broken = False
            try:
                while True:
                    chunk = remux.stdout.read(1 << 16)
                    if not chunk:
                        break
                    proc.stdin.write(chunk)
            except (BrokenPipeError, OSError):
                # The encoder is gone. Kill the remux first: it is blocked writing the rest of the
                # clip into a stdout pipe nobody drains any more, so wait() alone would hang forever.
                broken = True
                log("[aitv] encoder pipe broke; restarting encoder")
            finally:
                if broken:
                    remux.kill()
                try:
                    if remux.stdout:
                        remux.stdout.close()
                except OSError:
                    pass
                try:
                    remux.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    remux.kill()
                    try:
                        remux.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        pass
            if broken:
                _stop(proc)
                proc = _start_encoder(segment_seconds, window)
    except KeyboardInterrupt:
        pass
    finally:
        _stop(proc)
