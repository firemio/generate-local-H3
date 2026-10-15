"""Rundown: what the channel airs next.

The broadcast day is the LINEUP, slot by slot, looping: for each slot a *complete* programme made
for it (all of its clips rendered) airs from start to end — a new one as soon as it exists, and
once everything has aired the archive rotates, newer days more often — then a short break (a
commercial or two and an ident). A slot that has no finished programme yet is filled with a block
of the older short programmes of the same format, so the channel never stalls while a 6-minute
programme is being rendered.
"""
from __future__ import annotations

import random

from .programs import BREAK_FORMATS, LINEUP

FACT_FORMATS = ("news", "weather", "variety")   # only air these when they were written from real material
FILLER_TARGET_S = 60.0


def airable(items: list[dict]) -> list[dict]:
    """Clips the channel may put on air: everything except news/weather/variety that predate the real
    sources (they were invented) — those stay on disk but off the rundown."""
    return [it for it in items if it.get("sources") or it.get("format") not in FACT_FORMATS]


def programmes(items: list[dict]) -> list[dict]:
    """Group clips into programmes: by programme_id when the producer wrote one, else adjacent clips with
    the same format and title (the older one-script-per-clip library)."""
    out: list[dict] = []
    by_id: dict[str, dict] = {}
    for it in items:
        pid = it.get("programme_id")
        if pid:
            p = by_id.get(pid)
            if p is None:
                p = by_id[pid] = {"id": pid, "format": it.get("format", ""), "program": it.get("program", ""),
                                  "slot": it.get("slot"), "created": it.get("created", ""), "seconds": 0.0,
                                  "total": int(it.get("segments_total") or 0), "clips": []}
                out.append(p)
            p["clips"].append(it)
            p["seconds"] += float(it.get("seconds") or 0)
            continue
        last = out[-1] if out else None
        if last and not last.get("total") and last["format"] == it.get("format") and last["program"] == it.get("program"):
            last["clips"].append(it)
            last["seconds"] += float(it.get("seconds") or 0)
        else:
            out.append({"id": it.get("id") or it["file"], "format": it.get("format", ""), "program": it.get("program", ""),
                        "slot": None, "created": it.get("created", ""), "seconds": float(it.get("seconds") or 0),
                        "total": 0, "clips": [it]})
    for p in out:
        p["clips"].sort(key=lambda c: (int(c.get("segment_index") or 0), c.get("created", "")))
        p["complete"] = (not p["total"]) or len(p["clips"]) >= p["total"]
    return out


class Rundown:
    def __init__(self, rng: random.Random | None = None, lineup: list[str] | None = None):
        self.rng = rng or random.Random()
        self.lineup = lineup or list(LINEUP)
        self.slot = 0
        self.aired: set[str] = set()      # programme ids aired at least once this session
        self.recent: list[str] = []       # programme ids, most recent last

    def _pick(self, pool: list[dict], prefer_fresh: bool = True) -> dict | None:
        if not pool:
            return None
        if prefer_fresh:
            fresh = [p for p in pool if p["id"] not in self.aired]
            if fresh:
                return max(fresh, key=lambda p: p["created"])
        avoid = set(self.recent[-max(1, min(len(pool) - 1, len(pool) // 2)):])
        cand = [p for p in pool if p["id"] not in avoid] or pool
        ranked = sorted(cand, key=lambda p: p["created"])  # newer = heavier
        weights = [1.0 + i / max(1, len(ranked) - 1) * 2.0 for i in range(len(ranked))]
        return self.rng.choices(ranked, weights=weights)[0]

    def _take(self, p: dict) -> list[dict]:
        self.aired.add(p["id"])
        self.recent.append(p["id"])
        self.recent[:] = self.recent[-80:]
        return list(p["clips"])

    def _break(self, breaks: list[dict]) -> list[dict]:
        clips: list[dict] = []
        cms = [p for p in breaks if p["format"] == "cm"]
        idents = [p for p in breaks if p["format"] == "ident"]
        for _ in range(self.rng.choice((1, 2)) if cms else 0):
            cm = self._pick(cms)
            cms = [p for p in cms if p["id"] != cm["id"]]
            clips += self._take(cm)
        if idents:
            clips += self._take(self._pick(idents))
        return clips

    def next_block(self, items: list[dict]) -> list[dict]:
        """Ordered clips for the next slot of the day (programme + break); empty when the library is empty."""
        progs = [p for p in programmes(airable(items)) if p["complete"]]
        if not progs:
            return []
        breaks = [p for p in progs if p["format"] in BREAK_FORMATS]
        mains = [p for p in progs if p["format"] not in BREAK_FORMATS]
        slot = self.slot
        self.slot = (slot + 1) % len(self.lineup)
        fmt = self.lineup[slot]

        # 1. a full-length programme made for this slot: a new one airs first (newest first); once every one
        #    has aired, older days come back in the mix, newer ones more often — the archive is not wasted
        full = [p for p in mains if p["slot"] == slot and p["format"] == fmt]
        if full:
            return self._take(self._pick(full)) + self._break(breaks)

        # 2. nothing finished for this slot yet: a filler block of the short programmes of the same format
        #    (or of anything, when there are none), the usual fresh-first / weighted-random way
        short = [p for p in mains if p["format"] == fmt and p["slot"] is None] or [p for p in mains if p["slot"] is None] or mains
        clips: list[dict] = []
        seconds = 0.0
        pool = list(short)
        while pool and seconds < FILLER_TARGET_S:
            p = self._pick(pool)
            pool = [q for q in pool if q["id"] != p["id"]]
            clips += self._take(p)
            seconds += p["seconds"]
        return clips + self._break(breaks)
