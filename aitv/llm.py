"""Tiny chat client for local LLMs.

- Ollama (default, http://127.0.0.1:11434): uses the native /api/chat so we can force JSON output and pin the
  model to CPU (AITV_LLM_NUM_GPU=0, default) — the iGPU is busy with H3 and the 85 GB HIP ceiling is close.
- Anything OpenAI-compatible (LM Studio http://127.0.0.1:1234/v1, vLLM, ...): /chat/completions.
"""
from __future__ import annotations

import json
import os
import re
import urllib.request


class LLM:
    def __init__(self, base_url: str | None = None, model: str | None = None, timeout: float = 1800,
                 num_gpu: int | None = None):
        self.base_url = (base_url or os.environ.get("AITV_LLM_URL") or "http://127.0.0.1:11434").rstrip("/")
        self.model = model or os.environ.get("AITV_LLM_MODEL") or "gemma4:latest"
        self.timeout = timeout
        self.is_ollama = ":11434" in self.base_url
        if num_gpu is None:
            num_gpu = int(os.environ.get("AITV_LLM_NUM_GPU", "0"))
        self.num_gpu = num_gpu

    def _post(self, url: str, payload: dict) -> dict:
        req = urllib.request.Request(url, data=json.dumps(payload).encode(),
                                     headers={"Content-Type": "application/json",
                                              "Authorization": "Bearer " + os.environ.get("AITV_LLM_KEY", "local")})
        with urllib.request.urlopen(req, timeout=self.timeout) as r:
            return json.loads(r.read())

    def chat(self, system: str, user: str, json_mode: bool = True, temperature: float = 0.9) -> str:
        messages = [{"role": "system", "content": system}, {"role": "user", "content": user}]
        if self.is_ollama:
            root = self.base_url[:-3] if self.base_url.endswith("/v1") else self.base_url
            payload = {"model": self.model, "messages": messages, "stream": False,
                       "options": {"temperature": temperature, "num_ctx": 8192, "num_gpu": self.num_gpu}}
            if json_mode:
                payload["format"] = "json"
            return self._post(root + "/api/chat", payload)["message"]["content"]
        base = self.base_url if self.base_url.endswith("/v1") else self.base_url + "/v1"
        payload = {"model": self.model, "messages": messages, "temperature": temperature, "stream": False}
        if json_mode:
            payload["response_format"] = {"type": "json_object"}
        return self._post(base + "/chat/completions", payload)["choices"][0]["message"]["content"]

    def chat_json(self, system: str, user: str, retries: int = 3) -> dict:
        last = ""
        for _ in range(retries):
            last = self.chat(system, user, json_mode=True)
            obj = extract_json(last)
            if obj is not None:
                return obj
        raise ValueError("LLM did not return JSON: " + last[:400])

    def alive(self) -> bool:
        try:
            url = (self.base_url[:-3] if self.base_url.endswith("/v1") else self.base_url) + "/api/tags" if self.is_ollama \
                else (self.base_url if self.base_url.endswith("/v1") else self.base_url + "/v1") + "/models"
            with urllib.request.urlopen(url, timeout=5) as r:
                return r.status == 200
        except Exception:
            return False


def extract_json(text: str):
    text = text.strip()
    text = re.sub(r"^```(?:json)?\s*|\s*```$", "", text, flags=re.S)
    try:
        return json.loads(text)
    except Exception:
        pass
    m = re.search(r"\{.*\}", text, flags=re.S)
    if m:
        try:
            return json.loads(m.group(0))
        except Exception:
            return None
    return None
