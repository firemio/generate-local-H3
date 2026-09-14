"""Real-world material for the writer: the JMA forecast and NHK RSS headlines.

Generation stays fully local; these are the only network reads and they are best-effort: every
fetch has a short timeout, results are cached, and a failure just means "no material" so the
station picks another programme instead of inventing news.
"""
from __future__ import annotations

import dataclasses
import json
import os
import random
import re
import threading
import time
import urllib.request
import xml.etree.ElementTree as ET

JMA_AREA = os.environ.get("AITV_JMA_AREA", "130000")  # 130000 = 東京地方
JMA_OVERVIEW = "https://www.jma.go.jp/bosai/forecast/data/overview_forecast/{area}.json"
JMA_FORECAST = "https://www.jma.go.jp/bosai/forecast/data/forecast/{area}.json"
NHK_RSS = "https://www.nhk.or.jp/rss/news/{cat}.xml"
NHK_CATS = {"cat0": "主要", "cat1": "社会", "cat3": "科学・文化", "cat5": "経済", "cat6": "国際", "cat7": "スポーツ"}

OFFLINE = os.environ.get("AITV_OFFLINE", "") not in ("", "0")
TIMEOUT = 15.0
_cache: dict[str, tuple[float, object]] = {}
_lock = threading.Lock()


@dataclasses.dataclass
class Material:
    """What the writer gets: a text block for the LLM prompt, the raw items for template scripts,
    and short references kept in index.json so a viewer can see where a programme came from."""
    text: str
    items: list[dict]
    refs: list[str]


def _get(url: str) -> bytes:
    req = urllib.request.Request(url, headers={"User-Agent": "aitv/0.1 (local AI TV station)"})
    with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
        return r.read()


def _cached(key: str, ttl: float, fn):
    with _lock:
        hit = _cache.get(key)
        if hit and time.time() - hit[0] < ttl:
            return hit[1]
    if OFFLINE:
        return None
    try:
        val = fn()
    except Exception:
        val = None
    with _lock:
        # keep a stale value rather than nothing when the refresh fails
        if val is not None or key not in _cache:
            _cache[key] = (time.time(), val)
        return _cache[key][1]


# ---- JMA -------------------------------------------------------------------------------------
def _parse_forecast(overview: dict, forecast: list) -> dict:
    today = forecast[0]["timeSeries"]
    weather = today[0]["areas"][0]
    days = []
    for t, w in zip(today[0]["timeDefines"], weather.get("weathers", [])):
        days.append({"date": t[:10], "weather": re.sub(r"\s+", " ", w)})
    temps = {}
    if len(today) > 2 and today[2]["areas"]:
        ta = today[2]["areas"][0]
        values, seen = [], set()
        # JMA lists (min, max) per day and repeats today's max when the min is already past
        for i, (t, v) in enumerate(zip(today[2]["timeDefines"], ta.get("temps", []))):
            label = f"{t[:10]} {'最高' if i % 2 else '最低'} {v}℃"
            if v and (t[:10], v) not in seen:
                seen.add((t[:10], v))
                values.append(label)
        temps = {"point": ta["area"]["name"], "values": values}
    pops = []
    if len(today) > 1 and today[1]["areas"]:
        pa = today[1]["areas"][0]
        pops = [f"{t[11:16]} {p}%" for t, p in zip(today[1]["timeDefines"], pa.get("pops", [])) if p]
    text = re.sub(r"\s+", " ", overview.get("text", "")).strip()
    return {"area": weather["area"]["name"], "office": overview.get("publishingOffice", ""),
            "report": overview.get("reportDatetime", ""), "overview": text[:400],
            "days": days, "temps": temps, "pops": pops[:8]}


def jma_forecast(area: str = JMA_AREA) -> dict | None:
    def fetch():
        ov = json.loads(_get(JMA_OVERVIEW.format(area=area)))
        fc = json.loads(_get(JMA_FORECAST.format(area=area)))
        return _parse_forecast(ov, fc)
    return _cached(f"jma:{area}", 3600, fetch)


# ---- NHK RSS ---------------------------------------------------------------------------------
def nhk_headlines(cat: str = "cat0") -> list[dict] | None:
    def fetch():
        root = ET.fromstring(_get(NHK_RSS.format(cat=cat)))
        items = []
        for it in root.findall("./channel/item"):
            title = (it.findtext("title") or "").strip()
            if not title:
                continue
            items.append({"title": title, "description": re.sub(r"\s+", " ", it.findtext("description") or "").strip(),
                          "link": (it.findtext("link") or "").strip(), "pub": (it.findtext("pubDate") or "").strip(),
                          "category": NHK_CATS.get(cat, cat)})
        return items
    return _cached(f"nhk:{cat}", 1200, fetch)


def _headline_block(items: list[dict]) -> str:
    lines = []
    for i, it in enumerate(items, 1):
        desc = f" — {it['description'][:120]}" if it.get("description") else ""
        lines.append(f"{i}. [{it['category']}] {it['title']}{desc}")
    return "\n".join(lines)


# ---- per-format material ---------------------------------------------------------------------
def material_for(fmt_key: str, rng: random.Random | None = None) -> Material | None:
    """Real material for formats that need it; None when the format is fiction-only or nothing could be fetched."""
    rng = rng or random
    if fmt_key == "news":
        items = (nhk_headlines("cat0") or [])[:6]
        if not items:
            return None
        text = ("Today's real NHK headlines (Japanese). Use them faithfully: read them as given, condense but never "
                "add facts, names or numbers that are not in the headline.\n" + _headline_block(items))
        return Material(text, items, [it["title"] for it in items])
    if fmt_key == "weather":
        fc = jma_forecast()
        if not fc or not fc["days"]:
            return None
        lines = [f"Area: {fc['area']} (JMA {fc['office']}, issued {fc['report'][:16]})"]
        lines += [f"{d['date']}: {d['weather']}" for d in fc["days"]]
        if fc["temps"].get("values"):
            lines.append(f"Temperatures at {fc['temps']['point']}: " + ", ".join(fc["temps"]["values"]))
        if fc["pops"]:
            lines.append("Chance of rain: " + ", ".join(fc["pops"]))
        if fc["overview"]:
            lines.append("Overview: " + fc["overview"][:300])
        text = "Real JMA forecast. State only what is here (weather, temperatures, rain chance); no invented details.\n" + "\n".join(lines)
        items = [dict(d, area=fc["area"]) for d in fc["days"]]
        return Material(text, items, [f"{fc['area']} {d['date']} {d['weather']}" for d in fc["days"][:2]])
    if fmt_key == "variety":
        pool = []
        for cat in ("cat7", "cat3"):
            pool += (nhk_headlines(cat) or [])[:8]
        if not pool:
            return None
        picks = rng.sample(pool, min(3, len(pool)))
        text = ("Light real topics from today's NHK news (sports, science, culture) the hosts can chat or quiz about. "
                "Mention them by subject only; never portray or name real people.\n" + _headline_block(picks))
        return Material(text, picks, [it["title"] for it in picks])
    return None
