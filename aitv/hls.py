"""Broadcaster: turns the library into one continuous live HLS stream with ffmpeg.

A single long-running ffmpeg reads MPEG-TS from stdin (-re = real-time pacing) and writes a sliding
HLS window to aitv/hls/live.m3u8. Python feeds it clip after clip (remuxed to TS, stream copy),
picking the next programme with the rundown policy in pick_next() and picking up new clips as the
producer adds them. Any HLS player (the web page via hls.js, VLC, OBS Media Source) can tune in.
"""
from __future__ import annotations

import os
import random
import subprocess
import time

from . import index_store

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LIB = index_store.LIB
HLS = os.path.join(ROOT, "aitv", "hls")


def pick_next(items: list[dict], played_ever: set, recent: list[str], rng: random.Random) -> dict:
    """Rundown policy for a channel whose library grows slowly (one clip per ~400 s of GPU):
    1. anything never aired goes first (newest first) — fresh programmes air as soon as they render;
    2. otherwise a weighted random pick that avoids the most recent clips (no back-to-back repeats)
       and favours newer material, so a long-running channel keeps a mix instead of one fixed loop."""
    fresh = [it for it in items if it["file"] not in played_ever]
    if fresh:
        return sorted(fresh, key=lambda it: it.get("created", ""), reverse=True)[0]
    avoid = set(recent[-max(1, min(len(items) - 1, len(items) // 2)):])
    pool = [it for it in items if it["file"] not in avoid] or items
    ranked = sorted(pool, key=lambda it: it.get("created", ""))  # newer = heavier
    weights = [1.0 + i / max(1, len(ranked) - 1) * 2.0 for i in range(len(ranked))]
    return rng.choices(ranked, weights=weights)[0]


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
    played_ever: set = set()
    recent: list[str] = []
    rng = random.Random()
    warned_empty = False
    try:
        while True:
            if proc.poll() is not None:  # encoder died: replace it and keep the channel going
                log(f"[aitv] HLS encoder exited ({proc.returncode}); restarting")
                _stop(proc)
                proc = _start_encoder(segment_seconds, window)
            items = index_store.playable()  # only clips whose file is on disk
            if not items:
                if not warned_empty:
                    log("[aitv] library empty, waiting for the producer...")
                    warned_empty = True
                time.sleep(5)
                continue
            warned_empty = False
            it = pick_next(items, played_ever, recent, rng)
            played_ever.add(it["file"])
            recent.append(it["file"])
            recent[:] = recent[-50:]
            src = os.path.join(LIB, it["file"])
            log(f"[aitv] >> {it['program']} / {it['caption']}")
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
