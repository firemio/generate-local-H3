"""Minimal ComfyUI HTTP/WebSocket client with per-node timing (profiler)."""
from __future__ import annotations

import json
import os
import time
import uuid
import urllib.request
import urllib.parse
from dataclasses import dataclass, field
from typing import Optional

try:
    import websocket  # websocket-client
except ImportError:  # pragma: no cover
    websocket = None


@dataclass
class RunResult:
    prompt_id: str
    ok: bool
    seconds: float
    node_times: dict = field(default_factory=dict)  # display node id -> seconds
    outputs: list = field(default_factory=list)  # list of {filename, subfolder, type}
    error: Optional[str] = None
    local_files: list = field(default_factory=list)


class ComfyClient:
    def __init__(self, host: str = "127.0.0.1", port: int = 8188, client_id: Optional[str] = None,
                 output_dir: Optional[str] = None):
        self.host, self.port = host, port
        self.base = f"http://{host}:{port}"
        self.client_id = client_id or uuid.uuid4().hex
        # local ComfyUI output directory (scripts/run_comfyui.ps1 sets --output-directory <repo>/output)
        if output_dir is None and host in ("127.0.0.1", "localhost"):
            output_dir = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "output")
        self.output_dir = output_dir if output_dir and os.path.isdir(output_dir) else None

    # ---- basic HTTP ---------------------------------------------------------
    def _get(self, path: str, timeout: float = 30):
        with urllib.request.urlopen(self.base + path, timeout=timeout) as r:
            return json.loads(r.read())

    def _post(self, path: str, payload: dict, timeout: float = 60):
        data = json.dumps(payload).encode()
        req = urllib.request.Request(self.base + path, data=data, headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read())

    def alive(self) -> bool:
        try:
            self._get("/system_stats", timeout=5)
            return True
        except Exception:
            return False

    def system_stats(self) -> dict:
        return self._get("/system_stats")

    def object_info(self, node: str) -> dict:
        return self._get(f"/object_info/{node}")

    def free(self, unload_models: bool = True, free_memory: bool = True) -> None:
        try:
            self._post("/free", {"unload_models": unload_models, "free_memory": free_memory})
        except Exception:
            pass

    # ---- uploads ------------------------------------------------------------
    def upload(self, path: str, kind: str = "image", subfolder: str = "h3gen", overwrite: bool = True) -> str:
        """Upload an image/audio file to ComfyUI's input dir; returns the name to use in LoadImage/LoadAudio."""
        boundary = "----h3gen" + uuid.uuid4().hex
        name = os.path.basename(path)
        with open(path, "rb") as fh:
            blob = fh.read()
        parts = []
        parts.append(f"--{boundary}\r\nContent-Disposition: form-data; name=\"image\"; filename=\"{name}\"\r\n"
                     f"Content-Type: application/octet-stream\r\n\r\n".encode() + blob + b"\r\n")
        for k, v in (("subfolder", subfolder), ("overwrite", "true" if overwrite else "false"), ("type", "input")):
            parts.append(f"--{boundary}\r\nContent-Disposition: form-data; name=\"{k}\"\r\n\r\n{v}\r\n".encode())
        body = b"".join(parts) + f"--{boundary}--\r\n".encode()
        req = urllib.request.Request(self.base + "/upload/image", data=body,
                                     headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
        with urllib.request.urlopen(req, timeout=120) as r:
            info = json.loads(r.read())
        sub = info.get("subfolder") or ""
        return f"{sub}/{info['name']}" if sub else info["name"]

    # ---- run ----------------------------------------------------------------
    def queue(self, graph: dict) -> str:
        res = self._post("/prompt", {"prompt": graph, "client_id": self.client_id})
        if "error" in res:
            raise RuntimeError(json.dumps(res, ensure_ascii=False)[:2000])
        return res["prompt_id"]

    def run(self, graph: dict, timeout: float = 3 * 3600, log=print, download_dir: Optional[str] = None) -> RunResult:
        """Queue graph, follow websocket events, time each node, return outputs."""
        if websocket is None:
            raise RuntimeError("pip install websocket-client")
        ws = websocket.WebSocket()
        ws.connect(f"ws://{self.host}:{self.port}/ws?clientId={self.client_id}", timeout=30)
        ws.settimeout(60)
        t0 = time.time()
        pid = self.queue(graph)
        log(f"[h3gen] queued {pid}")
        node_times: dict[str, float] = {}
        cur, cur_t = None, t0
        ok, err = False, None
        last_progress = ("", -1)
        t_exec = None  # first executing event for this prompt (excludes queue wait)
        while time.time() - t0 < timeout:
            try:
                msg = ws.recv()
            except websocket.WebSocketTimeoutException:
                if not self.alive():
                    err = "server unreachable"
                    break
                continue
            if isinstance(msg, (bytes, bytearray)):
                continue  # preview images
            try:
                ev = json.loads(msg)
            except Exception:
                continue
            typ, data = ev.get("type"), ev.get("data", {})
            if data.get("prompt_id") not in (None, pid):
                continue
            now = time.time()
            if typ == "executing":
                node = data.get("node")
                if t_exec is None and node is not None:
                    t_exec = now
                if cur is not None:
                    node_times[cur] = node_times.get(cur, 0.0) + (now - cur_t)
                cur, cur_t = node, now
                if node is None:
                    ok = err is None
                    break
                log(f"[h3gen] {now - t0:7.1f}s executing {node}")
            elif typ == "progress":
                v, m = data.get("value"), data.get("max")
                if (data.get("node"), v) != last_progress:
                    last_progress = (data.get("node"), v)
                    log(f"[h3gen] {now - t0:7.1f}s   step {v}/{m}")
            elif typ == "execution_error":
                err = f"{data.get('node_type')}: {data.get('exception_message')}"
                log(f"[h3gen] ERROR {err}")
                log("\n".join(data.get("traceback", [])[-15:]))
            elif typ == "execution_interrupted":
                err = "interrupted"
                break
        ws.close()
        total = time.time() - (t_exec or t0)  # wall time of execution only (queue wait excluded)
        outputs, local = [], []
        try:
            hist = self._get(f"/history/{pid}").get(pid, {})
            for nid, out in hist.get("outputs", {}).items():
                for key in ("videos", "gifs", "images", "audio"):
                    for item in out.get(key, []) or []:
                        outputs.append(item)
            if hist.get("status", {}).get("status_str") == "error":
                ok = False
                err = err or "history reports error"
        except Exception as e:  # noqa
            log(f"[h3gen] history fetch failed: {e}")
        for item in outputs:
            # ComfyUI runs on this machine: resolve the file directly in its output directory.
            # (Never re-download into the same folder: /view + aiohttp sendfile on Windows is flaky, and a
            #  colliding destination path would truncate the freshly rendered file.)
            if self.output_dir:
                p = os.path.join(self.output_dir, item.get("subfolder", ""), item["filename"])
                if os.path.isfile(p):
                    local.append(p)
                    continue
            if download_dir:
                os.makedirs(download_dir, exist_ok=True)
                q = urllib.parse.urlencode({"filename": item["filename"], "subfolder": item.get("subfolder", ""),
                                            "type": item.get("type", "output")})
                dst = os.path.join(download_dir, item["filename"])
                if os.path.abspath(dst) in [os.path.abspath(x) for x in local]:
                    continue
                with urllib.request.urlopen(self.base + "/view?" + q, timeout=600) as r:
                    data = r.read()
                with open(dst, "wb") as fh:
                    fh.write(data)
                local.append(dst)
        return RunResult(pid, ok and not err, total, node_times, outputs, err, local)
