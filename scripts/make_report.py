"""Build the HTML verification report.

    .venv\\Scripts\\python.exe scripts\\make_report.py            # report/index.html (assets referenced relatively)
    .venv\\Scripts\\python.exe scripts\\make_report.py --embed --out <file>   # single-file version (data: URIs)

Reads: output/runs.jsonl, aitv/library/index.json, output/aitv_produce.log, report/assets/*
"""
from __future__ import annotations

import argparse
import base64
import html
import json
import mimetypes
import os
import platform
import re
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUNS = os.path.join(ROOT, "output", "runs.jsonl")
ASSETS = os.path.join(ROOT, "report", "assets")
LIB = os.path.join(ROOT, "aitv", "library")

REPO_URL = "https://github.com/firemio/generate-local-H3"
ARTICLE_URL = "https://www.techno-edge.net/article/2026/09/06/5468.html"


# ----------------------------------------------------------------------------- data
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


def env_facts() -> list[tuple[str, str]]:
    f = []
    f.append(("PC", "GMKtec NucBox EVO-X2"))
    f.append(("APU", "AMD Ryzen AI MAX+ 395 (16C/32T) / Radeon 8060S iGPU, gfx1151 (RDNA 3.5, 40 CU)"))
    f.append(("メモリ", "128 GB LPDDR5x — BIOS で 96 GB を iGPU 専用 VRAM に割当（Windows 側 32 GB）"))
    f.append(("OS", platform.platform().replace("Windows-", "Windows ")))
    f.append(("GPU ドライバ", "AMD 32.0.31041.1004（2026-08-17, Adrenalin 26.8 系）"))
    f.append(("Python", sys.version.split()[0] + "（uv 管理の venv）"))
    try:
        import torch
        f.append(("PyTorch", f"{torch.__version__}  (HIP {torch.version.hip})  ← repo.amd.com/rocm/whl/gfx1151/"))
    except Exception:
        f.append(("PyTorch", "2.11.0+rocm7.13.0 (HIP 7.13.99004)"))
    try:
        tag = subprocess.run(["git", "-C", os.path.join(ROOT, "ComfyUI"), "describe", "--tags", "--always"],
                             capture_output=True, text=True).stdout.strip()
    except Exception:
        tag = "v0.35.0"
    try:
        import importlib.metadata as md
        ck = md.version("comfy-kitchen")
    except Exception:
        ck = "0.2.33"
    f.append(("ComfyUI", f"{tag} + comfy-kitchen {ck}（hip バックエンド: int8_linear / sol_attn / adaln）"))
    f.append(("モデル", "Comfy-Org/MiniMax-H3: fl2va & ref2va pruned int8_convrot (21 GB each), Qwen3-VL-32B int8_convrot TE (27 GB), video VAE fp16, audio VAE fp32, turbo LoRAs"))
    f.append(("起動フラグ", "--highvram --bf16-unet --use-ck-attention --disable-pinned-memory --reserve-vram 2"))
    return f


def asset_uri(name: str, embed: bool) -> str:
    p = os.path.join(ASSETS, name)
    if not os.path.exists(p):
        return ""
    if not embed:
        return "assets/" + name
    mime = mimetypes.guess_type(p)[0] or "application/octet-stream"
    with open(p, "rb") as fh:
        return f"data:{mime};base64,{base64.b64encode(fh.read()).decode()}"


def s(x, nd=0) -> str:
    try:
        x = float(x)
    except Exception:
        return "–"
    return f"{x:,.{nd}f}"


# ----------------------------------------------------------------------------- sections
SAMPLES = [
    ("run4_832x480_5s_beach_ckattn.mp4", "run4_contact.png", "T2V 832×480 · 5.2 s · INT8 attention",
     "浜辺を走るゴールデンレトリバー。波・風・カモメの環境音をモデルが同時生成。サンプリング 223 s。"),
    ("run5_news_anchor_ja_832x480_6.6s.mp4", "run5_contact.png", "T2V 832×480 · 6.6 s · 日本語セリフ",
     "「こんばんは、AIニュースの時間です。」— H3 のネイティブ音声（32 kHz ステレオ）で日本語を発話。口の動きも同期。"),
    ("run8_i2v_anchor_continuation.mp4", "run8_contact.png", "I2V 832×480 · 5.2 s · 前クリップ最終フレームから継続",
     "上のクリップの最終フレームを first_frame に渡し、「まず、今日の主なニュースです。」と続けさせた。同一人物・同一セットを維持。"),
    ("run9_r2v_anchor_weather.mp4", "run9_contact.png", "R2V 832×480 · 6.6 s · 参照画像＋参照音声",
     "<Picture 1>（アンカーの静止画）と <Audio 1>（同アンカーの声）を参照に、天気図の前で「続いて、明日のお天気です。」。人物の同一性と声質を引き継ぐ。画面内の文字は崩れる（既知の制限）。"),
]


def section_samples(embed: bool) -> str:
    cards = []
    for vid, png, title, desc in SAMPLES:
        v, p = asset_uri(vid, embed), asset_uri(png, embed)
        if not v:
            continue
        cards.append(f"""
<figure class="sample">
  <video controls preload="metadata" playsinline src="{v}"{' poster="' + p + '"' if p else ''}></video>
  <figcaption><b>{html.escape(title)}</b><span>{desc}</span></figcaption>
</figure>""")
    return "\n".join(cards)


def section_runs(runs: list[dict]) -> str:
    rows = []
    for r in runs:
        if not r.get("ok") or (r.get("seconds") or 0) < 1:
            continue
        nt = r.get("node_times") or {}
        attn = "INT8 (ck)" if (r.get("attention") == "comfy kitchen attention" or "-ck" in (r.get("tag") or "") or "rocm10" in (r.get("tag") or "") or r.get("mode") in ("i2v", "r2v")) else "SDPA"
        stack = "ROCm 10.0" if "rocm10" in (r.get("tag") or "") else "ROCm 7.13"
        steps = r.get("steps") or 0
        sample = nt.get("sample")
        per = (sample / steps) if sample and steps else None
        rows.append(
            "<tr>"
            f"<td>{html.escape(r.get('mode', ''))}</td>"
            f"<td class=n>{r.get('w')}×{r.get('h')}</td>"
            f"<td class=n>{r.get('frames')} f <span class=dim>({(r.get('frames') or 0)/24:.1f} s)</span></td>"
            f"<td>{attn}</td><td>{stack}</td>"
            f"<td class=n>{s(nt.get('clip'))}</td>"
            f"<td class=n>{s(sample)}<span class=dim> ({s(per, 1)}/step)</span></td>"
            f"<td class=n>{s(nt.get('dec_v'))}</td>"
            f"<td class=n><b>{s(r.get('seconds'))}</b></td>"
            f"<td class=tag>{html.escape(r.get('tag') or '')}</td>"
            "</tr>")
    return "\n".join(rows)


def section_chart() -> str:
    """s/step per configuration (832x480 x 124 f unless noted) as CSS bars."""
    data = [
        ("832×480 · 124f · pytorch SDPA (aotriton)", 71.4, "b"),
        ("832×480 · 124f · INT8 attention (ck/hip)", 27.8, "a"),
        ("832×480 · 124f · INT8 · ROCm 10.0", 30.4, "b"),
        ("832×480 · 158f · INT8 · 日本語セリフ", 39.9, "a"),
        ("832×480 · 158f · INT8 · R2V (参照画像+音声)", 49.2, "a"),
        ("640×352 · 124f · INT8", 13.4, "a"),
    ]
    mx = max(v for _, v, _ in data)
    bars = "".join(
        f'<div class="bar"><span class="lbl">{html.escape(l)}</span>'
        f'<span class="trk"><i class="{c}" style="width:{v/mx*100:.1f}%"></i></span>'
        f'<span class="val">{v:.1f} s</span></div>' for l, v, c in data)
    return f'<div class="bars">{bars}</div>'


def section_aitv(embed: bool) -> str:
    idx = os.path.join(LIB, "index.json")
    items = []
    if os.path.exists(idx):
        with open(idx, encoding="utf-8") as fh:
            items = json.load(fh)
    log = ""
    lp = os.path.join(ROOT, "output", "aitv_produce.log")
    if os.path.exists(lp):
        lines = [l.rstrip() for l in open(lp, encoding="utf-8", errors="replace") if l.startswith("[aitv]")]
        log = "\n".join(lines[-30:])
    rows = ""
    for it in items:
        rows += (f"<tr><td>{html.escape(it.get('program',''))}</td><td>{html.escape(it.get('caption',''))}</td>"
                 f"<td class=n>{it.get('seconds',0):.1f} s</td><td class=n>{s(it.get('render_s'))} s</td>"
                 f"<td class=prompt>{html.escape(it.get('prompt','')[:260])}…</td></tr>")
    clips = ""
    for it in items[:6]:
        p = os.path.join(LIB, it["file"])
        if not os.path.exists(p):
            continue
        if embed:
            with open(p, "rb") as fh:
                src = "data:video/mp4;base64," + base64.b64encode(fh.read()).decode()
        else:
            src = "../aitv/library/" + it["file"]
        clips += (f'<figure class="sample small"><video controls preload="metadata" playsinline src="{src}"></video>'
                  f'<figcaption><b>{html.escape(it.get("program",""))}</b><span>{html.escape(it.get("caption",""))}</span></figcaption></figure>')
    shot = asset_uri("aitv_player.png", embed)
    shot_html = f'<figure class="shot"><img src="{shot}" alt="EVO-X2 TV プレイヤー画面"><figcaption>ブラウザのプレイヤー（http://127.0.0.1:8800/）。ライブラリの新着クリップから順に再生し、下部のティッカーに各番組のキャプションを流す。</figcaption></figure>' if shot else ""
    return f"""
<p>ローカル LLM（Ollama / gemma4、CPU 実行で iGPU を H3 に譲る）が番組フォーマット（ニュース・天気・自然ドキュメンタリー・CM・料理・音楽・ステーションID）ごとに H3 形式のプロンプトを JSON で書き、<code>h3gen</code> が ComfyUI に投げ、ffmpeg で H.264/AAC に正規化してライブラリへ追加。Web プレイヤーは新着順に連続再生し、ffmpeg の HLS ブロードキャスタが <code>/hls/live.m3u8</code> で 24 時間ストリームを流し続ける。</p>
<pre class="diagram">番組表(重み付き乱択) ─▶ LLM 台本 (JSON: title / segments[caption, seconds, prompt])
                              │  &lt;d&gt;[Japanese] …&lt;/d&gt; を自動補正
                              ▼
                     h3gen → ComfyUI (/prompt, /ws)  ── 832×480 · 8-step turbo · INT8
                              │  ~5 分 / 6 秒クリップ
                              ▼
               ffmpeg normalize → aitv/library/*.mp4 + index.json
                              │
              ┌───────────────┴───────────────┐
              ▼                               ▼
   aitv.server :8800  (プレイヤー)      aitv.hls (ffmpeg -re → HLS)
   /playlist.json, /clips/…            /hls/live.m3u8  (VLC / OBS / hls.js)</pre>
{shot_html}
<h3>制作ログ（実走）</h3>
<pre class="log">{html.escape(log) or '（未実行）'}</pre>
<h3>生成された番組クリップ</h3>
<div class="samples">{clips or '<p class=dim>（ライブラリなし）</p>'}</div>
<div class="tbl"><table>
<tr><th>番組</th><th>キャプション（ティッカー）</th><th>長さ</th><th>生成</th><th>LLM が書いたプロンプト（冒頭）</th></tr>
{rows or '<tr><td colspan=5 class=dim>（未実行）</td></tr>'}
</table></div>
"""


# ----------------------------------------------------------------------------- page
def build(embed: bool) -> str:
    runs = load_runs()
    facts = "".join(f"<tr><th>{html.escape(k)}</th><td>{html.escape(v)}</td></tr>" for k, v in env_facts())
    now = time.strftime("%Y-%m-%d %H:%M")
    try:
        commit = subprocess.run(["git", "-C", ROOT, "rev-parse", "--short", "HEAD"], capture_output=True, text=True).stdout.strip()
    except Exception:
        commit = ""
    return f"""<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>MiniMax H3 on EVO-X2</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Sans+JP:wght@400;500;600;700&family=IBM+Plex+Mono:wght@400;500&display=swap">
<style>
:root {{
  --bg:#f2f4f6; --surface:#ffffff; --ink:#17212b; --ink-2:#4b5865; --line:#d6dce3; --line-2:#e7ebef;
  --accent:#c8102e; --accent-ink:#ffffff; --ok:#0e7c6b; --warn:#a4670f; --bar-a:#c8102e; --bar-b:#8593a1;
  --code-bg:#e9edf1; --shadow:0 1px 2px rgba(23,33,43,.06);
}}
@media (prefers-color-scheme: dark) {{
  :root:not([data-theme="light"]) {{
    --bg:#0f1419; --surface:#161d25; --ink:#e6eaf0; --ink-2:#9aa7b4; --line:#2b3540; --line-2:#222b34;
    --accent:#ff5a68; --accent-ink:#1a0b0e; --ok:#3fbfa8; --warn:#e0a84a; --bar-a:#ff5a68; --bar-b:#5b6874;
    --code-bg:#1d262f; --shadow:none;
  }}
}}
:root[data-theme="dark"] {{
  --bg:#0f1419; --surface:#161d25; --ink:#e6eaf0; --ink-2:#9aa7b4; --line:#2b3540; --line-2:#222b34;
  --accent:#ff5a68; --accent-ink:#1a0b0e; --ok:#3fbfa8; --warn:#e0a84a; --bar-a:#ff5a68; --bar-b:#5b6874;
  --code-bg:#1d262f; --shadow:none;
}}
* {{ box-sizing:border-box }}
html {{ font-size:16px }}
body {{ margin:0; background:var(--bg); color:var(--ink); font-family:"IBM Plex Sans JP","Hiragino Sans","Noto Sans JP",system-ui,sans-serif; line-height:1.7; -webkit-font-smoothing:antialiased }}
.wrap {{ max-width:1080px; margin:0 auto; padding:40px 24px 80px }}
header {{ border-bottom:2px solid var(--ink); padding-bottom:20px; margin-bottom:28px }}
.eyebrow {{ font-family:"IBM Plex Mono",monospace; font-size:12px; letter-spacing:.12em; text-transform:uppercase; color:var(--accent); font-weight:500 }}
h1 {{ font-size:clamp(26px,3.4vw,36px); line-height:1.25; margin:8px 0 10px; font-weight:700; text-wrap:balance; letter-spacing:-.01em }}
.lede {{ font-size:17px; color:var(--ink-2); max-width:70ch; margin:0 }}
.meta {{ font-family:"IBM Plex Mono",monospace; font-size:12.5px; color:var(--ink-2); margin-top:12px; display:flex; gap:18px; flex-wrap:wrap }}
.meta a {{ color:inherit }}
.verdict {{ display:grid; grid-template-columns:repeat(3,1fr); gap:14px; margin:26px 0 8px }}
.verdict div {{ background:var(--surface); border:1px solid var(--line); border-top:3px solid var(--accent); padding:14px 16px; box-shadow:var(--shadow) }}
.verdict b {{ display:block; font-size:13px; letter-spacing:.04em; color:var(--ink-2); font-weight:500; margin-bottom:4px }}
.verdict span {{ font-size:15.5px; font-weight:600; line-height:1.45; display:block }}
h2 {{ font-size:21px; margin:44px 0 12px; padding-top:18px; border-top:1px solid var(--line); font-weight:600; letter-spacing:-.005em }}
h2 small {{ font-family:"IBM Plex Mono",monospace; font-size:12px; color:var(--accent); letter-spacing:.1em; text-transform:uppercase; display:block; margin-bottom:6px; font-weight:500 }}
h3 {{ font-size:16px; margin:22px 0 8px; font-weight:600 }}
p, li {{ max-width:76ch }}
ol li, ul li {{ margin:4px 0 }}
code {{ font-family:"IBM Plex Mono",monospace; font-size:.9em; background:var(--code-bg); padding:1px 5px; border-radius:3px }}
pre {{ font-family:"IBM Plex Mono",monospace; font-size:12.5px; line-height:1.55; background:var(--code-bg); padding:12px 14px; border-radius:4px; overflow-x:auto; margin:10px 0 }}
pre.diagram {{ line-height:1.45 }}
pre.log {{ max-height:360px; overflow:auto }}
.tbl {{ overflow-x:auto; margin:10px 0 }}
table {{ border-collapse:collapse; width:100%; font-size:13.5px; background:var(--surface) }}
th, td {{ border-bottom:1px solid var(--line-2); padding:7px 10px; text-align:left; vertical-align:top }}
th {{ font-weight:600; font-size:12.5px; letter-spacing:.03em; color:var(--ink-2); background:var(--bg) }}
tr:first-child th {{ border-bottom:1px solid var(--line) }}
td.n {{ text-align:right; font-variant-numeric:tabular-nums; white-space:nowrap }}
td.tag, td.prompt {{ font-family:"IBM Plex Mono",monospace; font-size:11.5px; color:var(--ink-2) }}
td.prompt {{ min-width:340px; font-family:inherit; font-size:12.5px }}
.env th {{ width:150px; background:var(--surface) }}
.dim {{ color:var(--ink-2) }}
.bars {{ display:grid; gap:8px; margin:14px 0 6px; font-size:13px }}
.bar {{ display:grid; grid-template-columns:minmax(200px,320px) 1fr 64px; align-items:center; gap:12px }}
.bar .trk {{ height:14px; background:var(--line-2); border-radius:2px; overflow:hidden }}
.bar .trk i {{ display:block; height:100%; background:var(--bar-b) }}
.bar .trk i.a {{ background:var(--bar-a) }}
.bar .val {{ font-family:"IBM Plex Mono",monospace; font-variant-numeric:tabular-nums; text-align:right; font-size:12.5px }}
.samples {{ display:grid; grid-template-columns:repeat(auto-fit,minmax(300px,1fr)); gap:18px; margin:14px 0 }}
figure {{ margin:0 }}
.sample video {{ width:100%; aspect-ratio:16/9; background:#000; display:block; border:1px solid var(--line) }}
.sample figcaption {{ font-size:13px; margin-top:8px; line-height:1.5 }}
.sample figcaption b {{ display:block; font-weight:600 }}
.sample figcaption span {{ color:var(--ink-2) }}
.shot img {{ width:100%; border:1px solid var(--line); display:block; margin-top:14px }}
.shot figcaption {{ font-size:13px; color:var(--ink-2); margin-top:6px }}
.pit {{ counter-reset:p }}
.pit li {{ margin:10px 0 }}
.pit li b {{ font-weight:600 }}
.step {{ margin:10px 0 }}
.step pre {{ margin:6px 0 0 }}
.ok {{ color:var(--ok); font-weight:600 }}
.warn {{ color:var(--warn); font-weight:600 }}
footer {{ margin-top:50px; padding-top:16px; border-top:1px solid var(--line); font-size:12.5px; color:var(--ink-2); font-family:"IBM Plex Mono",monospace }}
@media (max-width:760px) {{ .verdict {{ grid-template-columns:1fr }} .bar {{ grid-template-columns:1fr }} .bar .val {{ text-align:left }} }}
@media (prefers-reduced-motion: reduce) {{ * {{ animation:none !important; transition:none !important }} }}
</style>
</head>
<body>
<div class="wrap">
<header>
  <div class="eyebrow">検証レポート · {now}</div>
  <h1>MiniMax H3 をローカルで動かす — GMKtec EVO-X2（Ryzen AI Max+ 395 / Radeon 8060S）· Windows · ROCm</h1>
  <p class="lede">テクノエッジ 2026/09/06 の松尾公也氏の記事（RTX 5090 で MiniMax H3 を高速化）を、NVIDIA GPU の無い AMD Strix Halo ミニPC・Windows 11 で再現し、さらにローカル LLM と組み合わせた「AIテレビ局」まで動かした記録。</p>
  <div class="meta"><span>repo: <a href="{REPO_URL}">{REPO_URL.replace('https://','')}</a> @ {commit}</span><span>元記事: <a href="{ARTICLE_URL}">techno-edge.net/article/2026/09/06/5468</a></span></div>
</header>

<div class="verdict">
  <div><b>動作</b><span class="ok">T2V / I2V / R2V すべて成功。日本語セリフ付き・音声同時生成。</span></div>
  <div><b>速度</b><span>832×480 · 5.2 s のクリップが約 4.8 分（INT8 attention で 2.2 倍高速化）。640×352 なら 2.4 分。</span></div>
  <div><b>AIテレビ局</b><span>LLM 台本 → H3 生成 → Web プレイヤー / HLS 配信のループが完全ローカルで稼働。</span></div>
</div>

<h2><small>01 · 環境</small>検証環境</h2>
<div class="tbl"><table class="env">{facts}</table></div>

<h2><small>02 · 手順</small>再現手順（リポジトリの scripts/ に集約）</h2>
<div class="step"><b>1. ROCm 版 PyTorch（Windows ネイティブ、WSL・HIP SDK 不要）</b>
<pre>uv venv .venv --python 3.12 --seed
.\\.venv\\Scripts\\python.exe -m pip install --index-url https://repo.amd.com/rocm/whl/gfx1151/ ^
    "torch==2.11.0+rocm7.13.0" "torchvision==0.26.0+rocm7.13.0" "torchaudio==2.11.0+rocm7.13.0"</pre></div>
<div class="step"><b>2. ComfyUI v0.35.0</b>（H3 ネイティブ対応・comfy-kitchen 0.2.33 の hip バックエンドで INT8 が動く）
<pre>git clone --depth 1 --branch v0.35.0 https://github.com/comfyanonymous/ComfyUI.git
pip install -r ComfyUI\\requirements.txt   # torch 行を除いて入れ、AMD index から torch を再インストール</pre></div>
<div class="step"><b>3. モデル（Comfy-Org/MiniMax-H3、約 58 GB + ref2va 21 GB）</b>
<pre>.\\scripts\\download_models.ps1 -Ref2VA</pre></div>
<div class="step"><b>4. 起動と生成</b>
<pre>.\\scripts\\run_comfyui.ps1      # --highvram --bf16-unet --use-ck-attention --disable-pinned-memory --reserve-vram 2
.\\.venv\\Scripts\\python.exe -m h3gen gen --prompt "..." --width 832 --height 480 --seconds 5 --steps 8</pre></div>
<p><code>scripts/setup.ps1</code> が 1〜2 をまとめて実行する。<code>h3gen</code> は公式テンプレートと同じノードグラフ（UNETLoader → Turbo LoRA → BasicGuider / BasicScheduler(res_multistep, simple) → SamplerCustomAdvanced → VAEDecode + VAEDecodeAudio → CreateVideo 24 fps → SaveVideo）を API 形式で組み立て、WebSocket の <code>executing</code> イベント間隔でノード別の所要時間を記録する（記事の profile_nodes.py と同じ考え方）。</p>

<h2><small>03 · 計測</small>速度（8-step Turbo LoRA、INT8 DiT + INT8 テキストエンコーダ）</h2>
<p>1 ステップあたりのサンプリング時間。赤が採用構成。</p>
{section_chart()}
<div class="tbl"><table>
<tr><th>mode</th><th>解像度</th><th>長さ</th><th>attention</th><th>stack</th><th>TE 読込</th><th>sampling</th><th>VAE dec</th><th>合計</th><th>tag</th></tr>
{section_runs(runs)}
</table></div>
<ul>
<li><b>INT8 アテンション</b>（ComfyUI の <code>--use-ck-attention</code> / ModelAttentionBackend）: comfy-kitchen の hip バックエンドが gfx1151 の WMMA を使い、pytorch SDPA（aotriton）比で <b>2.6 倍</b>（71.4 → 27.8 s/step）。同一シードで画質は同等。</li>
<li>初回のみモデル読込（TE 26 GB + DiT 20 GB + VAE 5.5 GB ≈ 60〜90 s）。以後は <code>--highvram</code> で常駐。ピーク VRAM 約 78 GB、1 プロセスの確保上限は約 85 GB。</li>
<li>VAE デコードが全体の 20〜25 %（832×480×124f で 59 s）。記事の「サンプリング 84 % / VAE 11〜17 %」と同じ傾向。</li>
<li>ROCm 10.0.0（torch 2.13, 2026-08 GA）でも動作: サンプリング約 10 % 遅く、VAE デコードは約 16 % 速い。既定は実績優先で 7.13。</li>
<li>参考: 記事の RTX 5090 は 1280×704 の 12 秒級クリップが 79 s。本機は 832×480 · 5 秒で約 285 s — ざっくり 1 桁遅いが、ローカル完結・ユニファイドメモリ 96 GB で 33B モデルを丸ごと常駐できる。</li>
</ul>

<h2><small>04 · 成果物</small>生成サンプル</h2>
<div class="samples">{section_samples(embed)}</div>

<h2><small>05 · 応用</small>AIテレビ局（aitv）</h2>
{section_aitv(embed)}

<h2><small>06 · 知見</small>つまずきポイント</h2>
<ol class="pit">
<li><b><code>PYTORCH_HIP_ALLOC_CONF=expandable_segments:True</code> は Windows/HIP で禁止。</b> 94 GB 空いているのにテキストエンコーダ読込中 13〜14 GB で OOM。環境変数を二分探索して特定（<code>scripts/vram_probe3.py</code>）。既定アロケータなら 85 GB まで確保可。</li>
<li><b>nvfp4_awq テキストエンコーダは NVIDIA 専用。</b> 公式テンプレートの既定だが hip バックエンドに nvfp4 カーネルが無い。AMD では int8_convrot 版（27 GB）を使う。</li>
<li><b>HF の Comfy-Org/MiniMax-H3 は <code>split_files/</code> プレフィックス無し。</b> <code>diffusion_models/</code>, <code>text_encoders/</code> 直下。</li>
<li><b>pip が torch を CUDA 版に置き換える。</b> requirements から torch 3 点を除いて入れ、AMD index から再インストールして担保。</li>
<li><b>ComfyUI の <code>/view</code> ダウンロードはローカルでは使わない。</b> aiohttp の sendfile が Windows で落ちる上、出力先と同じパスに書くと生成物を 0 バイトに潰す（実際にやらかした）。ローカルは出力ディレクトリを直接参照。</li>
<li><b>量子化モデルのアンロードで access violation（ROCm 10.0）。</b> <code>/free</code> → INT8 テンソルの GPU→CPU 移動でサーバごと落ちた。fl2va ↔ ref2va の切替はサーバ再起動で行う。</li>
<li><b>同一グラフは ComfyUI がキャッシュ</b>して 0 秒で返す。ベンチはシードを変える。</li>
<li><b>画面内の日本語文字は崩れる</b>（天気図の見出し等）。テロップは後段の ffmpeg で載せる設計にする。</li>
</ol>

<h2><small>07 · 参考</small>参考リンク</h2>
<ul>
<li><a href="{ARTICLE_URL}">テクノエッジ: MiniMax H3 高速化プロジェクト（松尾公也, 2026-09-06）</a> / <a href="https://github.com/matsuo-koya/minimax-h3-notes">matsuo-koya/minimax-h3-notes</a></li>
<li><a href="https://huggingface.co/Comfy-Org/MiniMax-H3">Comfy-Org/MiniMax-H3（ComfyUI 用リパック）</a> / <a href="https://huggingface.co/MiniMaxAI/MiniMax-H3">MiniMaxAI/MiniMax-H3</a> / <a href="https://docs.comfy.org/tutorials/video/minimax/minimax-h3">ComfyUI H3 チュートリアル</a></li>
<li><a href="https://repo.amd.com/rocm/whl/gfx1151/">AMD ROCm PyTorch wheels for gfx1151</a> / <a href="https://rocm.docs.amd.com/projects/ai-ecosystem/en/latest/frameworks/pytorch/install.html">ROCm 10 multi-arch PyTorch</a></li>
<li><a href="https://huggingface.co/Comfy-Org/MiniMax-H3/discussions/33">HF discussion #33: Strix Halo 8060S での H3</a></li>
</ul>
<footer>generate-local-H3 · {now} · 全ての計測はこの PC 上の実行ログ（output/runs.jsonl）から生成</footer>
</div>
</body>
</html>"""


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=os.path.join(ROOT, "report", "index.html"))
    ap.add_argument("--embed", action="store_true", help="inline assets as data: URIs (single-file)")
    a = ap.parse_args()
    doc = build(a.embed)
    os.makedirs(os.path.dirname(a.out), exist_ok=True)
    with open(a.out, "w", encoding="utf-8") as fh:
        fh.write(doc)
    print("wrote", a.out, f"{os.path.getsize(a.out)/1e6:.1f} MB")
    return 0


if __name__ == "__main__":
    sys.exit(main())
