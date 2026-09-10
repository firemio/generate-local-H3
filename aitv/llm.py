"""Tiny OpenAI-compatible chat client (works with Ollama :11434/v1 and LM Studio :1234/v1)."""
from __future__ import annotations

import json
import os
import re
import urllib.request


class LLM:
    def __init__(self, base_url: str | None = None, model: str | None = None, timeout: float = 600):
        self.base_url = (base_url or os.environ.get("AITV_LLM_URL") or "http://127.0.0.1:11434/v1").rstrip("/")
        self.model = model or os.environ.get("AITV_LLM_MODEL") or "gemma4:latest"
        self.timeout = timeout

    def chat(self, system: str, user: str, json_mode: bool = True, temperature: float = 0.9) -> str:
        payload = {
            "model": self.model,
            "messages": [{"role": "system", "content": system}, {"role": "user", "content": user}],
            "temperature": temperature,
            "stream": False,
        }
        if json_mode:
            payload["response_format"] = {"type": "json_object"}
        req = urllib.request.Request(self.base_url + "/chat/completions", data=json.dumps(payload).encode(),
                                     headers={"Content-Type": "application/json",
                                              "Authorization": "Bearer " + os.environ.get("AITV_LLM_KEY", "local")})
        with urllib.request.urlopen(req, timeout=self.timeout) as r:
            res = json.loads(r.read())
        return res["choices"][0]["message"]["content"]

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
            with urllib.request.urlopen(self.base_url + "/models", timeout=5) as r:
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
