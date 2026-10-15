"""Two-pass programme writer: outline first, then the clips of each section.

The whole script is kept as aitv/library/scripts/<programme id>.json so a programme can be read as a
document (title, cast, sections, every clip's caption and prompt) and so the producer can resume it.
"""
from __future__ import annotations

import json
import os
import random
import re
import time
import uuid

from . import index_store
from .llm import LLM
from .programs import (FORMATS, SEGMENT_MAX_S, SEGMENT_MIN_S, THEME_COUNT, THEME_KEYS, THEMES, break_prompt,
                       outline_prompt, section_prompt)
from .sources import Material

SCRIPTS = os.path.join(index_store.LIB, "scripts")
LANG_TAG = "[Japanese]"
PLACEHOLDER_NAMES = {"不明", "未定", "N/A", "n/a", "なし", "TBD", "未設定", "名無し", "Anchor", "Host", "Presenter", "Narrator", "Chef", "Reporter"}
CAST_NAMES = ["佐藤 美咲", "高橋 悠真", "中村 千夏", "小林 蓮", "加藤 陽菜", "吉田 大和", "山本 結衣", "松本 拓海", "井上 澪",
              "木村 颯太", "林 さくら", "清水 隼人", "森 ひなた", "池田 航", "橋本 葵", "石川 湊"]
CHARS_PER_S = 5.5   # H3 speaks Japanese at roughly this rate; more and it rushes or cuts the line
_D = re.compile(r"<d>\[Japanese\]\s*(.*?)</d>", re.S)


def sanitize_prompt(prompt: str, lang_tag: str = LANG_TAG) -> str:
    """Make LLM output safe for H3: every <d>...</d> gets the language tag, [ja]-style tags are expanded,
    the three field names are spelled the way H3 expects, stray markdown fences are removed."""
    prompt = re.sub(r"^```(?:\w+)?\s*|\s*```$", "", prompt.strip(), flags=re.S)
    prompt = re.sub(r"\[/d\]|</d\s*>", "</d>", prompt)
    prompt = re.sub(r"</d>(\s*</d>)+", "</d>", prompt)
    prompt = re.sub(r"<d>\s*\[(ja|jp|japanese|日本語)\]\s*", f"<d>{lang_tag} ", prompt, flags=re.I)
    prompt = re.sub(r"<d>(?!\s*\[)\s*", f"<d>{lang_tag} ", prompt)
    prompt = re.sub(r"(?i)\bintegrated[-_ ]multimodal[-_ ]description\s*:", "integrated_multimodal_description:", prompt)
    prompt = re.sub(r"(?i)\boverall[-_ ]soundscape\s*:", "overall_soundscape:", prompt)
    prompt = re.sub(r"(?i)\bnon[-_ ]diegetic[-_ ]music\s*:", "non_diegetic_music:", prompt)
    prompt = _D.sub(lambda m: f"<d>{lang_tag} " + re.sub(r"[;；]\s*", "", m.group(1)).strip() + "</d>", prompt)
    return prompt


def speech_chars(prompt: str) -> int:
    return sum(len(m.strip()) for m in _D.findall(prompt))


def fit_speech(prompt: str, seconds: int) -> tuple[str, int]:
    """Give the dialogue room: lengthen the clip up to the maximum, then drop trailing sentences until
    what is left can be spoken in the time. Returns (prompt, seconds)."""
    chars = speech_chars(prompt)
    if chars <= CHARS_PER_S * seconds:
        return prompt, seconds
    seconds = max(seconds, min(SEGMENT_MAX_S, int(-(-chars // CHARS_PER_S))))
    budget = int(CHARS_PER_S * seconds)
    if chars <= budget:
        return prompt, seconds
    parts = _D.findall(prompt)
    kept_parts = []
    any_kept = False
    for p in parts:
        sents = re.findall(r"[^。！？!?]+[。！？!?]?", p.strip())
        kept = ""
        for s in sents:
            if len(kept) + len(s) > budget and (any_kept or kept):
                break  # the very first sentence of the clip is always kept, even if it is too long
            kept += s
            any_kept = True
        budget = max(0, budget - len(kept))
        kept_parts.append(kept.strip())
    it = iter(kept_parts)

    def repl(_m):
        t = next(it)
        return f"<d>{LANG_TAG} {t}</d>" if t else ""
    return _D.sub(repl, prompt), seconds


def _segments(raw: list, default_caption: str) -> list[dict]:
    out = []
    for s in raw or []:
        if not isinstance(s, dict) or not s.get("prompt"):
            continue
        try:
            secs = int(round(float(s.get("seconds") or 8)))
        except (TypeError, ValueError):
            secs = 8
        secs = max(SEGMENT_MIN_S, min(SEGMENT_MAX_S, secs))
        prompt, secs = fit_speech(sanitize_prompt(str(s["prompt"])), secs)
        out.append({"caption": str(s.get("caption") or default_caption).strip(), "seconds": secs, "prompt": prompt})
    return out


def _new_id() -> str:
    return time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:6]


def save(prog: dict) -> str:
    os.makedirs(SCRIPTS, exist_ok=True)
    path = os.path.join(SCRIPTS, prog["id"] + ".json")
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(prog, fh, ensure_ascii=False, indent=1)
    os.replace(tmp, path)
    return path


def history(fmt_key: str, n: int = 8) -> list[dict]:
    """The last n episodes of a format, oldest first: what the writer needs to not repeat itself and to
    carry the regulars and any teaser forward."""
    eps = []
    if not os.path.isdir(SCRIPTS):
        return eps
    rendered = {it.get("programme_id") for it in index_store.load()}  # a script nobody rendered never aired
    for name in os.listdir(SCRIPTS):
        if not name.endswith(".json"):
            continue
        try:
            with open(os.path.join(SCRIPTS, name), encoding="utf-8") as fh:
                p = json.load(fh)
        except (OSError, ValueError):
            continue
        if p.get("format") != fmt_key or not p.get("sections") or p.get("id") not in rendered:
            continue
        eps.append({"written": p.get("written", ""), "title": p.get("title", ""), "theme": p.get("theme", ""),
                    "summary": p.get("summary", ""), "next_time": p.get("next_time", ""),
                    "cast": p.get("cast", []), "sources": p.get("sources", []),
                    "text": " ".join([p.get("title", ""), p.get("theme", ""), p.get("summary", "")]
                                     + [str(sec.get("name", "")) + " " + str(sec.get("plan", "")) for sec in p["sections"]])})
    eps.sort(key=lambda e: e["written"])
    return eps[-n:]


def pick_themes(fmt_key: str, recent: list[dict], rng: random.Random | None = None) -> list[str]:
    """Least recently used themes of a format (random among the never-used ones)."""
    pool = THEMES.get(fmt_key)
    if not pool:
        return []
    rng = rng or random
    last_used = {}
    for i, e in enumerate(recent):
        hay = (e.get("text") or "").lower()
        for t in pool:
            if any(k.lower() in hay for k in THEME_KEYS.get(t, (t,))):
                last_used[t] = i
    never = [t for t in pool if t not in last_used]
    rng.shuffle(never)
    ranked = never + sorted(last_used, key=lambda t: last_used[t])
    # a teaser at the end of the last episode is a promise: whatever it named comes first; a teaser that
    # names no listed theme still wins over the rotation (the writer follows it instead), unless it is
    # just the previous teaser repeated (the promise was already kept or ignored)
    teaser = (recent[-1].get("next_time") or "").lower() if recent else ""
    repeated = len(recent) > 1 and teaser and teaser == (recent[-2].get("next_time") or "").lower()
    promised = [t for t in pool if teaser and not repeated and any(k.lower() in teaser for k in THEME_KEYS.get(t, (t,)))
                and last_used.get(t) != len(recent) - 1]
    if teaser and not repeated and not promised and THEME_COUNT.get(fmt_key, 1) == 1:
        return []
    ranked = promised + [t for t in ranked if t not in promised]
    return ranked[:THEME_COUNT.get(fmt_key, 1)]


def write_programme(llm: LLM, fmt_key: str, material: Material | None, minutes: float, slot: int | None = None,
                    log=print, rng: random.Random | None = None) -> dict:
    """Outline + every section. Raises if the LLM cannot produce a usable script."""
    fmt = FORMATS[fmt_key]
    mtext = material.text if material else None
    recent = history(fmt_key)
    regulars = [c for c in (recent[-1]["cast"] if recent else []) if isinstance(c, dict) and c.get("regular")]
    themes = pick_themes(fmt_key, recent, rng)
    reported = None
    if fmt_key == "news":
        cutoff = time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(time.time() - 36 * 3600))
        seen: list[str] = []
        for e in recent:
            if e["written"] >= cutoff:
                seen += [t for t in e.get("sources", []) if t not in seen]
        reported = seen[-40:] or None
    system, user = outline_prompt(fmt_key, minutes, mtext, regulars=regulars or None, recent=recent[-5:] or None,
                                  themes=themes or None, reported=reported)
    outline = llm.chat_json(system, user)
    sections = [s for s in outline.get("sections", []) if isinstance(s, dict) and s.get("name")]
    if not sections:
        raise ValueError("outline without sections")
    target = minutes * 60
    total = sum(float(s.get("seconds") or 0) for s in sections) or target
    for s in sections:
        s["seconds"] = max(20, int(round(float(s.get("seconds") or target / len(sections)) * target / total)))
    outline["sections"] = sections
    cast = [c for c in outline.get("cast", []) if isinstance(c, dict)]
    used = {c.get("name") for c in cast}
    for c in cast:  # a regular needs a real name to be carried from day to day
        if not str(c.get("name") or "").strip() or str(c.get("name")).strip() in PLACEHOLDER_NAMES:
            c["name"] = next((n for n in (rng or random).sample(CAST_NAMES, len(CAST_NAMES)) if n not in used), CAST_NAMES[0])
            used.add(c["name"])
    for r in regulars:  # the LLM sometimes drops or rewrites a regular: put the canonical one back
        if not any(c.get("name") == r.get("name") for c in cast):
            cast.insert(0, dict(r))
        else:
            for c in cast:
                if c.get("name") == r.get("name"):
                    c.update({k: r[k] for k in ("look", "voice", "role") if r.get(k)})
                    c["regular"] = True
    outline["cast"] = cast
    prog = {"id": _new_id(), "format": fmt_key, "slot": slot, "title": str(outline.get("title") or fmt["title"]).strip(),
            "theme": str(outline.get("theme") or (" / ".join(themes) if themes else "")).strip(),
            "summary": str(outline.get("summary") or "").strip(), "next_time": str(outline.get("next_time") or "").strip(),
            "cast": cast, "minutes": minutes,
            "sources": material.refs if material else [], "written": time.strftime("%Y-%m-%dT%H:%M:%S"), "sections": []}
    log(f"[aitv] outline: {prog['title']} [{prog['theme']}] — " + " / ".join(f"{s['name']}({s['seconds']}s)" for s in sections)
        + (f" — next: {prog['next_time']}" if prog["next_time"] else ""))
    prev = None
    for sec in sections:
        system, user = section_prompt(fmt_key, outline, sec, prev, mtext)
        try:
            res = llm.chat_json(system, user)
        except Exception as e:
            log(f"[aitv] section '{sec['name']}' failed ({e}); skipped")
            continue
        segs = _segments(res.get("segments"), sec["name"])
        if not segs:
            log(f"[aitv] section '{sec['name']}' came back empty; skipped")
            continue
        prog["sections"].append({"name": sec["name"], "plan": sec.get("plan"), "seconds": sec["seconds"], "segments": segs})
        prev = segs[-1]["caption"]
    if not prog["sections"]:
        raise ValueError("script without any section")
    prog["segments_total"] = sum(len(s["segments"]) for s in prog["sections"])
    prog["seconds_total"] = sum(g["seconds"] for s in prog["sections"] for g in s["segments"])
    save(prog)
    log(f"[aitv] script: {prog['title']}: {len(prog['sections'])} sections, {prog['segments_total']} clips, "
        f"{prog['seconds_total']/60:.1f} min -> scripts/{prog['id']}.json")
    return prog


def write_break(llm: LLM, fmt_key: str) -> dict:
    system, user = break_prompt(fmt_key, [e["title"] for e in history(fmt_key, 10) if e.get("title")] or None)
    res = llm.chat_json(system, user)
    segs = _segments(res.get("segments"), FORMATS[fmt_key]["title"])[:1]
    if not segs:
        raise ValueError("no segment for " + fmt_key)
    segs[0]["seconds"] = 4 if fmt_key == "ident" else 6
    prog = {"id": _new_id(), "format": fmt_key, "slot": None, "title": str(res.get("title") or FORMATS[fmt_key]["title"]).strip(),
            "cast": [], "minutes": segs[0]["seconds"] / 60, "sources": [], "written": time.strftime("%Y-%m-%dT%H:%M:%S"),
            "sections": [{"name": FORMATS[fmt_key]["title"], "plan": "", "seconds": segs[0]["seconds"], "segments": segs}],
            "segments_total": 1, "seconds_total": segs[0]["seconds"]}
    save(prog)
    return prog


def iter_segments(prog: dict):
    """(index, section name, segment) over the whole programme in air order."""
    i = 0
    for sec in prog["sections"]:
        for seg in sec["segments"]:
            yield i, sec["name"], seg
            i += 1
