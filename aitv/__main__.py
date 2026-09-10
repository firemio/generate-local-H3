"""CLI:
  python -m aitv produce [--count N] [--formats news,cm,...] [--no-llm] [--width 832 --height 480 --steps 8]
  python -m aitv serve  [--port 8800]                # web player + playlist + HLS files
  python -m aitv hls                                  # ffmpeg live HLS broadcaster (needs produced clips)
  python -m aitv station [--count N]                  # serve + hls + produce, all in one process
"""
from __future__ import annotations

import argparse
import os
import sys
import threading

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

from h3gen.client import ComfyClient  # noqa: E402
from .llm import LLM  # noqa: E402
from .producer import Producer  # noqa: E402
from .server import serve  # noqa: E402
from .hls import broadcast  # noqa: E402


def _producer(a) -> Producer:
    comfy = ComfyClient(a.comfy_host, a.comfy_port)
    if not comfy.alive():
        raise SystemExit("ComfyUI not reachable; start scripts\\run_comfyui.ps1")
    llm = None
    if not a.no_llm:
        llm = LLM(a.llm_url, a.llm_model, num_gpu=a.llm_gpu_layers)
        if not llm.alive():
            print(f"[aitv] LLM at {llm.base_url} not reachable -> using built-in fallback scripts")
            llm = None
        else:
            print(f"[aitv] writer LLM: {llm.model} @ {llm.base_url}")
    return Producer(comfy, llm, width=a.width, height=a.height, steps=a.steps, seed=a.seed)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(prog="aitv")
    sub = ap.add_subparsers(dest="cmd", required=True)

    def common(p):
        p.add_argument("--comfy-host", default="127.0.0.1")
        p.add_argument("--comfy-port", type=int, default=8188)
        p.add_argument("--llm-url", default=os.environ.get("AITV_LLM_URL", "http://127.0.0.1:11434"), help="Ollama root or an OpenAI-compatible /v1 base (LM Studio: http://127.0.0.1:1234/v1)")
        p.add_argument("--llm-gpu-layers", type=int, default=int(os.environ.get("AITV_LLM_NUM_GPU", "0")), help="Ollama num_gpu (0 = CPU only, keeps the iGPU for H3)")
        p.add_argument("--llm-model", default=os.environ.get("AITV_LLM_MODEL", "gemma4:latest"))
        p.add_argument("--no-llm", action="store_true")
        p.add_argument("--width", type=int, default=832)
        p.add_argument("--height", type=int, default=480)
        p.add_argument("--steps", type=int, default=8)
        p.add_argument("--seed", type=int)
        p.add_argument("--count", type=int, help="number of programmes to produce (default: forever)")
        p.add_argument("--formats", help="comma list, cycles in order (news,cm,nature,...)")

    p = sub.add_parser("produce"); common(p)
    s = sub.add_parser("serve"); s.add_argument("--host", default="127.0.0.1"); s.add_argument("--port", type=int, default=8800)
    h = sub.add_parser("hls"); h.add_argument("--segment", type=int, default=2)
    st = sub.add_parser("station"); common(st); st.add_argument("--host", default="127.0.0.1"); st.add_argument("--port", type=int, default=8800)
    st.add_argument("--no-hls", action="store_true")
    a = ap.parse_args(argv)

    if a.cmd == "produce":
        prod = _producer(a)
        prod.run(a.count, a.formats.split(",") if a.formats else None)
    elif a.cmd == "serve":
        serve(a.host, a.port)
    elif a.cmd == "hls":
        broadcast(a.segment)
    elif a.cmd == "station":
        serve(a.host, a.port, background=True)
        if not a.no_hls:
            threading.Thread(target=broadcast, daemon=True).start()
        prod = _producer(a)
        try:
            prod.run(a.count, a.formats.split(",") if a.formats else None)
            if a.count:
                print("[aitv] production finished; station keeps playing (Ctrl+C to stop)")
                threading.Event().wait()
        except KeyboardInterrupt:
            pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
