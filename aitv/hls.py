"""Broadcaster: turns the library into one continuous live HLS stream with ffmpeg.

A single long-running ffmpeg reads MPEG-TS from stdin (-re = real-time pacing) and writes a sliding
HLS window to aitv/hls/live.m3u8. Python feeds it clip after clip (remuxed to TS, stream copy),
looping over the playlist and picking up new clips as the producer adds them. Any HLS player
(the web page via hls.js, VLC, OBS Media Source) can tune in.
"""
from __future__ import annotations

import json
import os
import random
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


def pick_next(items: list[dict], played_ever: set, recent: list[str], rng: random.Random) -> dict:
    """Rundown policy for a channel whose library grows slowly:
    1. anything never aired goes first (newest first) — fresh programmes air as soon as they are rendered;
    2. otherwise a weighted random pick that avoids the most recent clips (no back-to-back repeats) and
       favours newer material, so a long-running channel keeps a mix instead of one fixed loop."""
    fresh = [it for it in items if it["file"] not in played_ever]
    if fresh:
        return sorted(fresh, key=lambda it: it.get("created", ""), reverse=True)[0]
    avoid = set(recent[-max(1, min(len(items) - 1, len(items) // 2)):])
    pool = [it for it in items if it["file"] not in avoid] or items
    # newer = heavier: rank by creation time
    ranked = sorted(pool, key=lambda it: it.get("created", ""))
    weights = [1.0 + i / max(1, len(ranked) - 1) * 2.0 for i in range(len(ranked))]
    return rng.choices(ranked, weights=weights)[0]


def _start_encoder(segment_seconds: int, window: int) -> subprocess.Popen:
    out = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-fflags", "+genpts+igndts",
           "-f", "mpegts", "-i", "pipe:0",
           "-c", "copy", "-f", "hls", "-hls_time", str(segment_seconds), "-hls_list_size", str(window),
           "-hls_flags", "delete_segments+append_list+omit_endlist+program_date_time",
           "-hls_segment_filename", os.path.join(HLS, "seg%06d.ts"), os.path.join(HLS, "live.m3u8")]
    return subprocess.Popen(out, stdin=subprocess.PIPE)


def broadcast(segment_seconds: int = 2, window: int = 10, log=print) -> None:
    os.makedirs(HLS, exist_ok=True)
    for f in os.listdir(HLS):
        try:
            os.remove(os.path.join(HLS, f))
        except OSError:
            pass
    proc = _start_encoder(segment_seconds, window)
    log("[aitv] HLS broadcaster started -> aitv/hls/live.m3u8")
    played_ever: set = set()
    recent: list[str] = []
    rng = random.Random()
    try:
        while True:
            if proc.poll() is not None:  # encoder died: restart it and keep going
                log(f"[aitv] HLS encoder exited ({proc.returncode}); restarting")
                proc = _start_encoder(segment_seconds, window)
            items = _items()
            if not items:
                log("[aitv] library empty, waiting for the producer...")
                time.sleep(5)
                continue
            it = pick_next(items, played_ever, recent, rng)
            played_ever.add(it["file"])
            recent.append(it["file"])
            recent[:] = recent[-50:]
            src = os.path.join(LIB, it["file"])
            if not os.path.exists(src):
                continue
            log(f"[aitv] ▶ {it['program']} / {it['caption']}")
            remux = subprocess.Popen(["ffmpeg", "-hide_banner", "-loglevel", "error", "-i", src,
                                      "-c:v", "copy", "-bsf:v", "h264_mp4toannexb", "-c:a", "copy",
                                      "-f", "mpegts", "pipe:1"], stdout=subprocess.PIPE)
            try:
                while True:
                    chunk = remux.stdout.read(1 << 16)
                    if not chunk:
                        break
                    proc.stdin.write(chunk)
            except (BrokenPipeError, OSError):
                log("[aitv] encoder pipe broke; will restart encoder")
            finally:
                remux.wait()
    except KeyboardInterrupt:
        pass
    finally:
        try:
            proc.stdin.close()
        except Exception:
            pass
        proc.terminate()
