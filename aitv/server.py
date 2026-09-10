"""Station web server: player page, rolling playlist JSON, clip files, optional HLS dir."""
from __future__ import annotations

import http.server
import json
import os
import socketserver
import threading
import urllib.parse

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LIB = os.path.join(ROOT, "aitv", "library")
WEB = os.path.join(ROOT, "aitv", "web")
HLS = os.path.join(ROOT, "aitv", "hls")


class Handler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, fmt, *args):  # quiet
        pass

    def _send(self, code: int, ctype: str, body: bytes, extra: dict | None = None):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Access-Control-Allow-Origin", "*")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path
        if path in ("/", "/index.html"):
            with open(os.path.join(WEB, "index.html"), "rb") as fh:
                return self._send(200, "text/html; charset=utf-8", fh.read())
        if path == "/playlist.json":
            from . import index_store
            items = index_store.load()   # reads bytes then parses; never holds the file open
            from .programs import STATION
            body = json.dumps({"station": STATION, "items": items[-200:]}, ensure_ascii=False).encode()
            return self._send(200, "application/json; charset=utf-8", body)
        if path.startswith("/clips/"):
            return self._serve_file(os.path.join(LIB, os.path.basename(path)), "video/mp4")
        if path.startswith("/hls/"):
            name = os.path.basename(path)
            ctype = "application/vnd.apple.mpegurl" if name.endswith(".m3u8") else "video/mp2t"
            return self._serve_file(os.path.join(HLS, name), ctype)
        return self._send(404, "text/plain", b"not found")

    def _serve_file(self, fpath: str, ctype: str):
        if not os.path.isfile(fpath):
            return self._send(404, "text/plain", b"not found")
        size = os.path.getsize(fpath)
        rng = self.headers.get("Range")
        start, end = 0, size - 1
        code = 200
        if rng and rng.startswith("bytes="):
            a, _, b = rng[6:].partition("-")
            start = int(a or 0)
            end = int(b) if b else size - 1
            code = 206
        length = end - start + 1
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Length", str(length))
        self.send_header("Access-Control-Allow-Origin", "*")
        if code == 206:
            self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
        self.end_headers()
        with open(fpath, "rb") as fh:
            fh.seek(start)
            remaining = length
            while remaining > 0:
                chunk = fh.read(min(1 << 20, remaining))
                if not chunk:
                    break
                try:
                    self.wfile.write(chunk)
                except (ConnectionResetError, BrokenPipeError, ConnectionAbortedError):
                    return
                remaining -= len(chunk)


class Server(socketserver.ThreadingTCPServer):
    # On Windows SO_REUSEADDR lets a SECOND process bind a port that is already being listened on,
    # so a duplicate station would start silently (two producers, two HLS encoders, one index.json).
    # Refuse instead: the second launch fails with WinError 10048 and says so.
    allow_reuse_address = os.name != "nt"
    daemon_threads = True


def serve(host: str = "127.0.0.1", port: int = 8800, background: bool = False):
    srv = Server((host, port), Handler)
    print(f"[aitv] station on http://{host}:{port}/   (playlist: /playlist.json, HLS: /hls/live.m3u8)")
    if background:
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        return srv
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return srv
