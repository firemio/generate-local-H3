"""CLI:  python -m h3gen gen|bench|info

Examples
  python -m h3gen gen --prompt "A cat walking on a beach, waves, seagulls" --seconds 4 --steps 8
  python -m h3gen gen --mode i2v --first-frame my.png --prompt "..." --seconds 5
  python -m h3gen gen --mode r2v --ref-image anchor.png --ref-audio voice.wav --prompt "<Picture 1> ... <d>[ja]こんにちは</d>"
  python -m h3gen bench --seconds 4 --steps 8 --runs 2
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time

from .client import ComfyClient
from .graph import DEFAULT_FILES, GenSpec, build, canvas

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUNS_LOG = os.path.join(ROOT, "output", "runs.jsonl")


def _add_common(p: argparse.ArgumentParser) -> None:
    p.add_argument("--host", default=os.environ.get("COMFY_HOST", "127.0.0.1"))
    p.add_argument("--port", type=int, default=int(os.environ.get("COMFY_PORT", "8188")))
    p.add_argument("--prompt", default="")
    p.add_argument("--prompt-file")
    p.add_argument("--mode", choices=["t2v", "i2v", "r2v"], default="t2v")
    p.add_argument("--aspect", default="16:9")
    p.add_argument("--mp", type=float, default=0.4, help="megapixels tier: 0.4 draft, 1.0 native")
    p.add_argument("--width", type=int)
    p.add_argument("--height", type=int)
    p.add_argument("--seconds", type=float, default=5.0)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--steps", type=int, default=8)
    p.add_argument("--no-turbo", action="store_true", help="disable turbo LoRA (use 20 steps)")
    p.add_argument("--lora-strength", type=float, default=1.0)
    p.add_argument("--shift-video", type=float)
    p.add_argument("--shift-audio", type=float)
    p.add_argument("--attention", choices=["pytorch attention", "comfy kitchen attention"])
    p.add_argument("--scheduler", default="simple")
    p.add_argument("--first-frame")
    p.add_argument("--last-frame")
    p.add_argument("--ref-image", action="append", default=[])
    p.add_argument("--ref-audio", action="append", default=[])
    p.add_argument("--ref-image-size", choices=["match", "max"], default="match")
    p.add_argument("--unet", help="override diffusion model filename")
    p.add_argument("--clip", help="override text encoder filename")
    p.add_argument("--prefix", default="h3/h3")
    p.add_argument("--download-dir", default=None, help="copy outputs here via /view (only needed for a remote ComfyUI)")
    p.add_argument("--tag", default="", help="free-form label written to runs.jsonl")
    p.add_argument("--dry-run", action="store_true", help="print the API graph and exit")


def spec_from_args(a, client: ComfyClient | None) -> GenSpec:
    prompt = a.prompt
    if a.prompt_file:
        with open(a.prompt_file, encoding="utf-8") as fh:
            prompt = fh.read()
    if not prompt:
        raise SystemExit("--prompt or --prompt-file required")
    w, h = (a.width, a.height) if a.width and a.height else canvas(a.aspect, a.mp)
    files = dict(DEFAULT_FILES)
    if a.unet:
        files["fl2va" if a.mode != "r2v" else "ref2va"] = a.unet
    if a.clip:
        files["clip"] = a.clip
    spec = GenSpec(prompt=prompt, mode=a.mode, width=w, height=h, seconds=a.seconds, seed=a.seed,
                   steps=a.steps, turbo=not a.no_turbo, lora_strength=a.lora_strength,
                   shift_video=a.shift_video, shift_audio=a.shift_audio, attention=a.attention,
                   scheduler=a.scheduler, ref_image_size=a.ref_image_size, filename_prefix=a.prefix, files=files)
    if client is not None and not a.dry_run:
        if a.first_frame:
            spec.first_frame = client.upload(a.first_frame)
        if a.last_frame:
            spec.last_frame = client.upload(a.last_frame)
        spec.ref_images = [client.upload(p) for p in a.ref_image]
        spec.ref_audios = [client.upload(p, kind="audio") for p in a.ref_audio]
    else:
        spec.first_frame, spec.last_frame = a.first_frame, a.last_frame
        spec.ref_images, spec.ref_audios = list(a.ref_image), list(a.ref_audio)
    return spec


def _record(entry: dict) -> None:
    os.makedirs(os.path.dirname(RUNS_LOG), exist_ok=True)
    with open(RUNS_LOG, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(entry, ensure_ascii=False) + "\n")


def cmd_gen(a) -> int:
    client = ComfyClient(a.host, a.port)
    spec = spec_from_args(a, client)
    graph = build(spec)
    if a.dry_run:
        print(json.dumps(graph, indent=1, ensure_ascii=False))
        return 0
    if not client.alive():
        raise SystemExit(f"ComfyUI not reachable at {client.base} (start scripts\\run_comfyui.ps1)")
    print(f"[h3gen] {spec.mode} {spec.width}x{spec.height} {spec.length}f ({spec.length/24:.2f}s) "
          f"steps={spec.steps} turbo={spec.turbo} seed={spec.seed}")
    res = client.run(graph, download_dir=a.download_dir)
    entry = {"ts": time.strftime("%Y-%m-%dT%H:%M:%S"), "tag": a.tag, "mode": spec.mode, "w": spec.width,
             "h": spec.height, "frames": spec.length, "steps": spec.steps, "turbo": spec.turbo,
             "seed": spec.seed, "unet": spec.files["ref2va" if spec.mode == "r2v" else "fl2va"],
             "clip": spec.files["clip"], "attention": spec.attention, "ok": res.ok, "seconds": round(res.seconds, 1),
             "node_times": {k: round(v, 1) for k, v in res.node_times.items()}, "outputs": res.local_files,
             "error": res.error, "prompt": spec.prompt[:300]}
    _record(entry)
    print(f"[h3gen] {'OK' if res.ok else 'FAILED'} in {res.seconds:.1f}s")
    for k, v in sorted(res.node_times.items(), key=lambda kv: -kv[1]):
        print(f"    {k:<10} {v:8.1f}s")
    for f in res.local_files:
        print(f"[h3gen] -> {f}")
    if res.error:
        print(f"[h3gen] error: {res.error}")
    return 0 if res.ok else 1


def cmd_bench(a) -> int:
    client = ComfyClient(a.host, a.port)
    if not client.alive():
        raise SystemExit("ComfyUI not reachable")
    if not a.prompt and not a.prompt_file:
        a.prompt = ("A golden retriever runs along a sunny beach toward the camera, waves rolling in, "
                    "seagulls overhead. Camera tracks low and steady. Audio: waves, wind, distant gulls, paws on wet sand.")
    times = []
    for i in range(a.runs):
        a.seed = a.seed + i
        spec = spec_from_args(a, client)
        res = client.run(build(spec), download_dir=a.download_dir)
        times.append(res.seconds)
        _record({"ts": time.strftime("%Y-%m-%dT%H:%M:%S"), "tag": a.tag or "bench", "run": i, "mode": spec.mode,
                 "w": spec.width, "h": spec.height, "frames": spec.length, "steps": spec.steps, "turbo": spec.turbo,
                 "attention": spec.attention, "ok": res.ok, "seconds": round(res.seconds, 1),
                 "node_times": {k: round(v, 1) for k, v in res.node_times.items()}, "error": res.error})
        print(f"[bench] run {i}: {'OK' if res.ok else 'FAIL'} {res.seconds:.1f}s  "
              + " ".join(f"{k}={v:.0f}s" for k, v in sorted(res.node_times.items(), key=lambda kv: -kv[1])[:4]))
        if not res.ok:
            print("[bench] error:", res.error)
            return 1
    if len(times) > 1:
        print(f"[bench] first(cold)={times[0]:.1f}s  warm mean={sum(times[1:])/len(times[1:]):.1f}s")
    return 0


def cmd_info(a) -> int:
    client = ComfyClient(a.host, a.port)
    print(json.dumps(client.system_stats(), indent=1))
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(prog="h3gen")
    sub = ap.add_subparsers(dest="cmd", required=True)
    g = sub.add_parser("gen"); _add_common(g); g.set_defaults(fn=cmd_gen)
    b = sub.add_parser("bench"); _add_common(b); b.add_argument("--runs", type=int, default=2); b.set_defaults(fn=cmd_bench)
    i = sub.add_parser("info"); i.add_argument("--host", default="127.0.0.1"); i.add_argument("--port", type=int, default=8188)
    i.set_defaults(fn=cmd_info)
    a = ap.parse_args(argv)
    return a.fn(a)


if __name__ == "__main__":
    sys.exit(main())
