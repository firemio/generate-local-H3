"""One loudness level for the whole channel.

H3 renders come out anywhere between -30 and -10 LUFS (a whispering narrator, then a shouting ident),
so every clip is normalised on its way into the library with ffmpeg's two-pass loudnorm: pass 1 measures,
pass 2 applies a plain gain (linear mode) unless the true-peak ceiling forces limiting.

Target: -16 LUFS integrated / -1.5 dBTP, the streaming level (Apple, Spotify, YouTube are -14..-16).
ARIB TR-B32 broadcast is -24 LKFS, which is too quiet for a browser player. Override with AITV_LUFS.

    python -m aitv renorm [--dry-run] [--limit N]    # bring the existing library to the target (audio only)
"""
from __future__ import annotations

import json
import os
import subprocess
import time

from . import index_store

LIB = index_store.LIB
TARGET_I = float(os.environ.get("AITV_LUFS", "-16"))
TARGET_TP = -1.5
TARGET_LRA = 11.0
TOLERANCE = 1.0     # LU; a clip within this of the target is left alone (renorm is idempotent)
SILENCE_I = -50.0   # below this there is no usable audio: +35 dB of gain would only raise hiss


def _params() -> str:
    return f"I={TARGET_I}:TP={TARGET_TP}:LRA={TARGET_LRA}"


def measure(path: str, timeout: int = 120) -> dict | None:
    """loudnorm pass 1. Returns the measured block (input_i, input_tp, ...) or None if it cannot be read."""
    cmd = ["ffmpeg", "-hide_banner", "-nostats", "-i", path, "-vn",
           "-af", f"loudnorm={_params()}:print_format=json", "-f", "null", "-"]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    except (OSError, subprocess.TimeoutExpired):
        return None
    err = r.stderr or ""
    start, end = err.rfind("{"), err.rfind("}")
    if start < 0 or end < start:
        return None
    try:
        m = json.loads(err[start:end + 1])
        return {k: float(m[k]) for k in ("input_i", "input_tp", "input_lra", "input_thresh", "target_offset")}
    except (ValueError, KeyError):
        return None


def filter_for(m: dict) -> str:
    """loudnorm pass 2 with the pass-1 measurement, so the correction is a constant gain where possible."""
    return (f"loudnorm={_params()}:measured_I={m['input_i']}:measured_TP={m['input_tp']}"
            f":measured_LRA={m['input_lra']}:measured_thresh={m['input_thresh']}"
            f":offset={m['target_offset']}:linear=true:print_format=none")


def audio_filter_args(src: str) -> list[str]:
    """ffmpeg output args that bring `src` to the target, or [] when there is nothing to correct."""
    m = measure(src)
    if m is None or m["input_i"] < SILENCE_I:
        return []
    return ["-af", filter_for(m)]


def renormalize_library(dry_run: bool = False, limit: int = 0, log=print) -> dict:
    """Bring every library clip to the target. Only the audio is re-encoded (video is stream-copied),
    ~1 s per clip. Safe to run while the station is on air: the clip being streamed is locked by the
    broadcaster's ffmpeg and is skipped (run again later; clips already at level are left alone)."""
    files = sorted(f for f in os.listdir(LIB) if f.endswith(".mp4"))
    if limit:
        files = files[:limit]
    stats = {"total": len(files), "fixed": 0, "at_level": 0, "silent": 0, "locked": 0, "failed": 0}
    t0 = time.time()
    for n, name in enumerate(files, 1):
        src = os.path.join(LIB, name)
        m = measure(src)
        if m is None:
            stats["failed"] += 1
            log(f"[renorm] {name}: could not measure")
            continue
        if m["input_i"] < SILENCE_I:
            stats["silent"] += 1
            continue
        if abs(m["input_i"] - TARGET_I) <= TOLERANCE:
            stats["at_level"] += 1
            continue
        log(f"[renorm] {n}/{len(files)} {name}: {m['input_i']:.1f} LUFS -> {TARGET_I:.0f}"
            + ("  (dry run)" if dry_run else ""))
        if dry_run:
            stats["fixed"] += 1
            continue
        tmp = src + ".renorm.tmp"
        cmd = ["ffmpeg", "-y", "-hide_banner", "-loglevel", "error", "-i", src,
               "-c:v", "copy", "-af", filter_for(m),
               "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2",
               "-f", "mp4", "-movflags", "+faststart", tmp]
        try:
            subprocess.run(cmd, check=True, timeout=300)
        except (OSError, subprocess.SubprocessError) as e:
            stats["failed"] += 1
            log(f"[renorm] {name}: ffmpeg failed ({e})")
            _rm(tmp)
            continue
        replaced = False
        for _ in range(4):  # the broadcaster holds a clip open for ~10 s while it remuxes it
            try:
                os.replace(tmp, src)
                replaced = True
                break
            except PermissionError:
                time.sleep(4)
        if replaced:
            stats["fixed"] += 1
        else:
            stats["locked"] += 1
            log(f"[renorm] {name}: in use by the broadcaster, skipped (run renorm again later)")
            _rm(tmp)
    log(f"[renorm] done in {time.time() - t0:.0f}s: " + ", ".join(f"{k} {v}" for k, v in stats.items()))
    return stats


def _rm(path: str) -> None:
    try:
        os.remove(path)
    except OSError:
        pass
