"""Broadcaster: turns the library into one continuous live HLS stream with ffmpeg.

A single long-running ffmpeg reads MPEG-TS from stdin (-re = real-time pacing) and writes a sliding
HLS window to aitv/hls/live.m3u8. Python feeds it clip after clip (remuxed to TS, stream copy),
looping over the playlist and picking up new clips as the producer adds them. Any HLS player
(the web page via hls.js, VLC, OBS Media Source) can tune in.
"""
from __future__ import annotations

import json
import os
import subprocess
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LIB = os.path.join(ROOT, "aitv", "library")
HLS = os.path.join(ROOT, "aitv", "hls")


def _items() -> list[dict]:
    idx = os.path.join(LIB, "index.json")
    if not os.path.exists(idx):
        return []
    with open(idx, encoding="utf-8") as fh:
        return json.load(fh)


def broadcast(segment_seconds: int = 2, window: int = 10, log=print) -> None:
    os.makedirs(HLS, exist_ok=True)
    for f in os.listdir(HLS):
        try:
            os.remove(os.path.join(HLS, f))
        except OSError:
            pass
    out = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-fflags", "+genpts+igndts",
           "-f", "mpegts", "-i", "pipe:0",
           "-c", "copy", "-f", "hls", "-hls_time", str(segment_seconds), "-hls_list_size", str(window),
           "-hls_flags", "delete_segments+append_list+omit_endlist+program_date_time",
           "-hls_segment_filename", os.path.join(HLS, "seg%06d.ts"), os.path.join(HLS, "live.m3u8")]
    proc = subprocess.Popen(out, stdin=subprocess.PIPE)
    log("[aitv] HLS broadcaster started -> aitv/hls/live.m3u8")
    played: list[str] = []
    try:
        while True:
            items = _items()
            if not items:
                log("[aitv] library empty, waiting for the producer...")
                time.sleep(5)
                continue
            # prefer clips not yet played this cycle; when exhausted, start a new cycle
            pending = [it for it in items if it["file"] not in played]
            if not pending:
                played.clear()
                pending = items
            it = pending[0]
            played.append(it["file"])
            src = os.path.join(LIB, it["file"])
            if not os.path.exists(src):
                continue
            log(f"[aitv] ▶ {it['program']} / {it['caption']}")
            remux = subprocess.Popen(["ffmpeg", "-hide_banner", "-loglevel", "error", "-i", src,
                                      "-c:v", "copy", "-bsf:v", "h264_mp4toannexb", "-c:a", "copy",
                                      "-f", "mpegts", "pipe:1"], stdout=subprocess.PIPE)
            while True:
                chunk = remux.stdout.read(1 << 16)
                if not chunk:
                    break
                proc.stdin.write(chunk)
            remux.wait()
    except (KeyboardInterrupt, BrokenPipeError):
        pass
    finally:
        try:
            proc.stdin.close()
        except Exception:
            pass
        proc.terminate()
