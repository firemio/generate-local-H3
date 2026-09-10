"""Producer loop: schedule -> LLM script -> H3 render -> library/playlist."""
from __future__ import annotations

import json
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
from .llm import LLM  # noqa: E402
from .programs import FORMATS, STATION, fallback_programme, pick_format, writer_prompt  # noqa: E402

LIB = os.path.join(ROOT, "aitv", "library")
INDEX = os.path.join(LIB, "index.json")


def load_index() -> list[dict]:
    if os.path.exists(INDEX):
        with open(INDEX, encoding="utf-8") as fh:
            return json.load(fh)
    return []


def save_index(items: list[dict]) -> None:
    os.makedirs(LIB, exist_ok=True)
    tmp = INDEX + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(items, fh, ensure_ascii=False, indent=1)
    os.replace(tmp, INDEX)


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
    subprocess.run(cmd, check=True)


class Producer:
    def __init__(self, comfy: ComfyClient, llm: LLM | None, width: int = 832, height: int = 480,
                 steps: int = 8, seed: int | None = None, log=print):
        self.comfy, self.llm = comfy, llm
        self.width, self.height, self.steps = width, height, steps
        self.rng = random.Random(seed)
        self.log = log
        self.last_format = None

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

    def render_segment(self, fmt_key: str, prog_title: str, seg: dict) -> dict | None:
        seconds = float(seg.get("seconds") or FORMATS[fmt_key]["seconds"])
        seconds = max(3.0, min(seconds, 10.0))
        seed = self.rng.randrange(1, 2**31)
        seg["prompt"] = sanitize_prompt(seg["prompt"])
        spec = GenSpec(prompt=seg["prompt"], mode="t2v", width=self.width, height=self.height, seconds=seconds,
                       seed=seed, steps=self.steps, turbo=True, filename_prefix="aitv/seg")
        t = time.time()
        res = self.comfy.run(build(spec), log=lambda *_: None)
        if not res.ok or not res.local_files:
            self.log(f"[aitv] render failed: {res.error}")
            return None
        uid = time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:6]
        dst = os.path.join(LIB, f"{fmt_key}-{uid}.mp4")
        os.makedirs(LIB, exist_ok=True)
        try:
            normalize(res.local_files[0], dst, self.width, self.height)
        except Exception as e:
            self.log(f"[aitv] ffmpeg normalize failed ({e}); copying raw")
            shutil.copy(res.local_files[0], dst)
        item = {"id": uid, "file": os.path.basename(dst), "format": fmt_key, "program": prog_title,
                "caption": seg.get("caption") or prog_title, "seconds": spec.length / 24.0, "seed": seed,
                "render_s": round(res.seconds, 1), "created": time.strftime("%Y-%m-%dT%H:%M:%S"),
                "prompt": seg["prompt"]}
        self.log(f"[aitv] + {item['file']}  ({item['seconds']:.1f}s clip, rendered in {res.seconds:.0f}s, "
                 f"sampling {res.node_times.get('sample', 0):.0f}s) 「{item['caption']}」")
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
                items = load_index()
                items.append(item)
                save_index(items)
        return made

    def wait_for_comfy(self, poll: float = 15.0) -> None:
        """Block until the ComfyUI API answers (server restart, driver reset, ...)."""
        warned = False
        while not self.comfy.alive():
            if not warned:
                self.log(f"[aitv] ComfyUI at {self.comfy.base} not reachable; waiting...")
                warned = True
            time.sleep(poll)
        if warned:
            self.log("[aitv] ComfyUI is back")

    def run(self, count: int | None = None, formats: list[str] | None = None) -> None:
        """Production loop. count=None -> run forever (24/7 station); every failure is logged and retried
        with a growing pause so one bad programme never stops the channel."""
        n, failures = 0, 0
        while count is None or n < count:
            fmt = formats[n % len(formats)] if formats else None
            try:
                self.wait_for_comfy()
                made = self.produce_one(fmt)
                n += 1
                failures = 0 if made else failures + 1
            except KeyboardInterrupt:
                raise
            except Exception as e:  # LLM/HTTP/ffmpeg/... keep the station alive
                failures += 1
                self.log(f"[aitv] programme failed ({type(e).__name__}: {e}); retrying")
            if failures:
                pause = min(300, 20 * failures)
                self.log(f"[aitv] {failures} consecutive failure(s); pausing {pause}s")
                time.sleep(pause)
