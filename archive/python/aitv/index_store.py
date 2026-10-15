"""Single owner of aitv/library/index.json.

Everything in the station (producer, HLS broadcaster, web server) goes through here so that:
  - readers never hold the file open while parsing (Windows refuses os.replace onto an open handle),
  - the writer retries the atomic replace instead of losing a clip that took ~400 s of GPU time,
  - in-process access is serialised by one lock.
"""
from __future__ import annotations

import json
import os
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LIB = os.path.join(ROOT, "aitv", "library")
INDEX = os.path.join(LIB, "index.json")

_lock = threading.Lock()


def load() -> list[dict]:
    """Read and parse. The handle is closed before parsing, so a concurrent replace can proceed."""
    try:
        with open(INDEX, "rb") as fh:
            data = fh.read()
    except FileNotFoundError:
        return []
    except OSError:
        return []
    if not data:
        return []
    try:
        return json.loads(data.decode("utf-8"))
    except Exception:
        return []  # torn read during a replace: the caller retries on its next pass


def save(items: list[dict], attempts: int = 40, pause: float = 0.05) -> None:
    """Write the index, never losing a clip that cost ~400 s of GPU.

    os.replace onto index.json fails with PermissionError on Windows while any other handle has it
    open (CPython opens without FILE_SHARE_DELETE). Readers here hold the handle for microseconds,
    but an external reader (an editor, a tail, a browser-side tool) can hold it for much longer, so
    after the retry budget the writer falls back to rewriting in place. A reader that catches that
    moment sees invalid JSON, and load() answers [] so the caller simply retries on its next pass."""
    os.makedirs(LIB, exist_ok=True)
    payload = json.dumps(items, ensure_ascii=False, indent=1).encode("utf-8")
    tmp = INDEX + ".tmp"
    with _lock:
        with open(tmp, "wb") as fh:
            fh.write(payload)
        last: Exception | None = None
        for _ in range(attempts):
            try:
                os.replace(tmp, INDEX)
                return
            except OSError as e:  # PermissionError [WinError 5] while a reader holds the file
                last = e
                time.sleep(pause)
        try:  # last resort: in-place rewrite (not atomic, but never drops the clip)
            with open(INDEX, "wb") as fh:
                fh.write(payload)
            try:
                os.remove(tmp)
            except OSError:
                pass
            return
        except OSError as e:
            raise RuntimeError(f"could not update index.json ({last}); in-place rewrite also failed: {e}")


def append(item: dict) -> None:
    with _lock:
        items = load()
    items.append(item)
    save(items)


def playable() -> list[dict]:
    """Index entries whose mp4 is actually on disk."""
    return [it for it in load() if it.get("file") and os.path.exists(os.path.join(LIB, it["file"]))]
