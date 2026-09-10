"""Build report/index.html from output/runs.jsonl + environment facts.

    .venv\\Scripts\\python.exe scripts\\make_report.py [--out report/index.html]
"""
from __future__ import annotations

import argparse
import html
import json
import os
import platform
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUNS = os.path.join(ROOT, "output", "runs.jsonl")


def load_runs() -> list[dict]:
    runs = []
    if os.path.exists(RUNS):
        for line in open(RUNS, encoding="utf-8"):
            line = line.strip()
            if line:
                try:
                    runs.append(json.loads(line))
                except Exception:
                    pass
    return runs


def env_facts() -> dict:
    facts = {"os": platform.platform(), "python": sys.version.split()[0]}
    try:
        import torch
        facts["torch"] = torch.__version__
        facts["hip"] = str(torch.version.hip)
        if torch.cuda.is_available():
            p = torch.cuda.get_device_properties(0)
            facts["gpu"] = f"{torch.cuda.get_device_name(0)} ({getattr(p, 'gcnArchName', '')})"
            facts["vram_total_gb"] = round(p.total_memory / 1e9, 1)
    except Exception as e:  # noqa
        facts["torch"] = f"n/a ({e})"
    try:
        out = subprocess.run(["git", "-C", os.path.join(ROOT, "ComfyUI"), "describe", "--tags", "--always"],
                             capture_output=True, text=True).stdout.strip()
        facts["comfyui"] = out
    except Exception:
        pass
    try:
        import importlib.metadata as md
        facts["comfy_kitchen"] = md.version("comfy-kitchen")
    except Exception:
        pass
    try:
        facts["commit"] = subprocess.run(["git", "-C", ROOT, "rev-parse", "--short", "HEAD"],
                                         capture_output=True, text=True).stdout.strip()
    except Exception:
        pass
    return facts


def fmt_s(x) -> str:
    try:
        x = float(x)
    except Exception:
        return "-"
    return f"{x:,.0f}s" if x >= 100 else f"{x:.1f}s"


def build_html(runs: list[dict], facts: dict, notes_md: str = "") -> str:
    ok_runs = [r for r in runs if r.get("ok") and r.get("seconds", 0) > 1]
    rows = []
    for r in runs:
        nt = r.get("node_times") or {}
        rows.append(
            "<tr>"
            f"<td>{html.escape(str(r.get('ts', '')))[5:16]}</td>"
            f"<td>{html.escape(str(r.get('tag') or ''))}</td>"
            f"<td>{html.escape(str(r.get('mode', '')))}</td>"
            f"<td class=num>{r.get('w')}×{r.get('h')}</td>"
            f"<td class=num>{r.get('frames')}f ({(r.get('frames') or 0)/24:.1f}s)</td>"
            f"<td class=num>{r.get('steps')}{' turbo' if r.get('turbo') else ''}</td>"
            f"<td class=num>{fmt_s(nt.get('clip'))}</td>"
            f"<td class=num>{fmt_s(nt.get('sample'))}</td>"
            f"<td class=num>{fmt_s(nt.get('dec_v'))}</td>"
            f"<td class=num><b>{fmt_s(r.get('seconds'))}</b></td>"
            f"<td>{'✅' if r.get('ok') else '❌ ' + html.escape(str(r.get('error') or '')[:80])}</td>"
            "</tr>")
    best = {}
    for r in ok_runs:
        key = (r.get("w"), r.get("h"), r.get("frames"), r.get("steps"))
        if key not in best or r["seconds"] < best[key]["seconds"]:
            best[key] = r
    summary = "".join(
        f"<li><b>{k[0]}×{k[1]}, {k[2]} frames ({k[2]/24:.1f}s), {k[3]} steps</b>: "
        f"{fmt_s(v['seconds'])} total, sampling {fmt_s((v.get('node_times') or {}).get('sample'))}, "
        f"VAE decode {fmt_s((v.get('node_times') or {}).get('dec_v'))}</li>"
        for k, v in sorted(best.items(), key=lambda kv: kv[0][0] * kv[0][1] * kv[0][2]))
    facts_rows = "".join(f"<tr><th>{html.escape(k)}</th><td>{html.escape(str(v))}</td></tr>" for k, v in facts.items())
    return f"""<!doctype html>
<html lang="ja"><head><meta charset="utf-8"><title>MiniMax H3 on EVO-X2 — 検証レポート</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
 body{{font-family:"Segoe UI","Hiragino Sans","Noto Sans JP",sans-serif;max-width:1100px;margin:32px auto;padding:0 20px;line-height:1.6;color:#1b1b1b;background:#fafafa}}
 h1{{font-size:26px}} h2{{font-size:20px;border-bottom:2px solid #ddd;padding-bottom:4px;margin-top:36px}}
 table{{border-collapse:collapse;width:100%;font-size:13.5px;background:#fff}} th,td{{border:1px solid #ddd;padding:6px 8px;text-align:left;vertical-align:top}}
 th{{background:#f0f0f0}} td.num{{text-align:right;font-variant-numeric:tabular-nums}}
 .ok{{color:#0a7}} .ng{{color:#c33}} code,pre{{background:#eee;border-radius:4px;padding:1px 5px;font-size:13px}} pre{{padding:10px;overflow-x:auto}}
 .card{{background:#fff;border:1px solid #e3e3e3;border-radius:8px;padding:14px 18px;margin:12px 0}}
 .grid{{display:grid;grid-template-columns:1fr 1fr;gap:14px}} @media(max-width:800px){{.grid{{grid-template-columns:1fr}}}}
 video{{width:100%;background:#000;border-radius:6px}}
</style></head><body>
<h1>MiniMax H3 をローカル（EVO-X2 / Radeon 8060S / Windows）で動かす — 検証レポート</h1>
<p>生成日時: {time.strftime('%Y-%m-%d %H:%M')} ／ リポジトリ: <code>generate-local-H3</code> commit <code>{html.escape(str(facts.get('commit','')))}</code></p>
{notes_md}
<h2>環境</h2>
<table>{facts_rows}</table>
<h2>ベスト計測（設定ごとの最速）</h2>
<ul>{summary or '<li>計測なし</li>'}</ul>
<h2>全実行ログ（output/runs.jsonl）</h2>
<div style="overflow-x:auto"><table>
<tr><th>時刻</th><th>tag</th><th>mode</th><th>解像度</th><th>長さ</th><th>steps</th><th>TE</th><th>sampling</th><th>VAE dec</th><th>合計</th><th>結果</th></tr>
{''.join(rows)}
</table></div>
</body></html>"""


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=os.path.join(ROOT, "report", "index.html"))
    ap.add_argument("--notes", default=os.path.join(ROOT, "report", "notes.html"),
                    help="optional HTML fragment inserted after the title")
    a = ap.parse_args()
    notes = open(a.notes, encoding="utf-8").read() if os.path.exists(a.notes) else ""
    doc = build_html(load_runs(), env_facts(), notes)
    os.makedirs(os.path.dirname(a.out), exist_ok=True)
    with open(a.out, "w", encoding="utf-8") as fh:
        fh.write(doc)
    print("wrote", a.out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
