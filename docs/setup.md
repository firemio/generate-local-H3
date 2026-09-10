# セットアップ詳細（手動で追う場合）

`scripts/setup.ps1` がやっていることの説明。全て Windows ネイティブ（WSL 不要、HIP SDK 不要）。

## 前提

- BIOS で iGPU への VRAM 割当を最大（EVO-X2 128 GB 機なら 96 GB）にしておく。
  Windows 側から見える物理メモリは 32 GB になる。
- AMD Software（Adrenalin）は 2026 年夏以降のもの（検証: 32.0.31041.1004）。
- Git, Python 3.12（uv で入る）, ffmpeg（PATH 上に）。

## 1. ROCm PyTorch（gfx1151 用 Windows wheel）

AMD が pip index として配布している。`rocm[libraries]` を別途入れる必要は無く、torch の依存として
`rocm-sdk-core` と `rocm-sdk-libraries-gfx1151`（合計 ~4 GB）が入る。

```powershell
uv venv .venv --python 3.12 --seed
.\.venv\Scripts\python.exe -m pip install --index-url https://repo.amd.com/rocm/whl/gfx1151/ `
    "torch==2.11.0+rocm7.13.0" "torchvision==0.26.0+rocm7.13.0" "torchaudio==2.11.0+rocm7.13.0"
.\.venv\Scripts\python.exe scripts\check_gpu.py
```

期待される出力:

```
torch      : 2.11.0+rocm7.13.0
hip        : 7.13.99004
device     : AMD Radeon(TM) 8060S Graphics gfx1151
total mem  : 115.8 GB
```

`total mem` が物理 VRAM 割当より大きく見えるのは HIP が共有分も報告するため。実際に確保できる上限は ~85 GB。

他の選択肢（未使用）:
- `rocm.nightlies.amd.com/v2/gfx1151/`: 開発版。torch 2.9.1+rocm7.13.0a2026042x。
- ROCm 7.2.1 公式 Windows PyTorch（repo.radeon.com）: torch 2.9.1、要ドライバ 26.2.2、cp312 のみ。

## 2. ComfyUI

```powershell
git clone --depth 1 --branch v0.35.0 https://github.com/comfyanonymous/ComfyUI.git ComfyUI
```

requirements.txt の `torch/torchvision/torchaudio` 行を除いて pip install し、最後に AMD index から torch を
再インストール（pip が CUDA 版に置き換えていないことを担保）。`comfy-kitchen==0.2.33` は Windows wheel があり、
ROCm 上では `hip` バックエンドが有効になる（INT8 GEMM / sol_attn / adaln）。

`config/extra_model_paths.yaml` を `ComfyUI/` にコピーすると `models/` 配下を参照する。

## 3. モデル

`scripts/download_models.ps1`（`huggingface_hub` + `hf_transfer`）。HF のパスは `diffusion_models/…` のように
フォルダ直下なので、`--include "vae/*"` のような指定になる。

## 4. 起動

`scripts/run_comfyui.ps1`。要点:

| 設定 | 理由 |
|---|---|
| `HIP_VISIBLE_DEVICES=0`, `CUDA_VISIBLE_DEVICES` 削除 | 空文字が残っていると HIP デバイスが隠れる |
| `PYTORCH_HIP_ALLOC_CONF` を **unset** | expandable_segments は Windows/HIP で 14 GB OOM（docs/pitfalls.md） |
| `--highvram` | 96 GB あるので TE(26 GB)+DiT(20 GB)+VAE(5.5 GB) を常駐 |
| `--bf16-unet` | gfx1151 は bf16 WMMA 対応、H3 は bf16 学習 |
| `--use-pytorch-cross-attention` | gfx1151 + torch 2.7+ は aotriton SDPA が使える（自動判定も通る） |
| `--disable-pinned-memory` | ユニファイドメモリ APU では pinned が逆効果という報告 |
| `--reserve-vram 2` | デスクトップ用に少し残す |

起動ログで確認する行:

```
[INFO] Found comfy_kitchen backend hip: {'available': True ...
[INFO] Total VRAM 110457 MB, total RAM 32407 MB
[INFO] AMD arch: gfx1151
[INFO] ROCm version: (7, 13)
[INFO] Set vram state to: HIGH_VRAM
[INFO] Using pytorch attention
```

## 5. 生成

```powershell
.\.venv\Scripts\python.exe -m h3gen gen --prompt "..." --width 832 --height 480 --seconds 5 --steps 8
```

`h3gen` は ComfyUI の `/prompt` に API 形式グラフを投げ、WebSocket の `executing` イベント間隔から
ノード別所要時間を出す（松尾氏の profile_nodes.py と同じ考え方）。結果は `output/runs.jsonl`。

## 6. 代替スタック: ROCm 10.0.0（torch 2.13, 2026-08-26 GA）

ComfyUI の README が案内する新しい配布形式（`torch[device-gfx1151]` extras）。別 venv で検証済み:

```powershell
uv venv .venv-rocm10 --python 3.12 --seed
.\.venv-rocm10\Scripts\python.exe -m pip install --index-url https://stable.repo.amd.com/rocm/whl-next/ `
    "torch[device-gfx1151]==2.13.0+rocm10.0.0" "torchvision[device-gfx1151]==0.28.0+rocm10.0.0" "torchaudio==2.11.0.2+rocm10.0.0"
# ComfyUI 依存は torch をピン留めして入れる
.\.venv-rocm10\Scripts\python.exe -m pip freeze | Select-String "^(torch|rocm|amd)" > torch-pin.txt
.\.venv-rocm10\Scripts\python.exe -m pip install -c torch-pin.txt -r ComfyUI\requirements.txt websocket-client
.\scripts\run_comfyui.ps1 -Venv .venv-rocm10
```

`torch.version.hip` は 7.15.26333、ComfyUI は `ROCm version: (7, 15)` と表示。comfy-kitchen の hip バックエンドも有効。

同一条件（832x480, 124f, 8-step turbo, INT8 attention, seed 2）の比較:

| スタック | サンプリング | VAE デコード |
|---|---|---|
| torch 2.11 + ROCm 7.13（既定） | 27.8 s/step (223 s) | 59.1 s |
| torch 2.13 + ROCm 10.0.0 | ~30.4 s/step (243 s) | 49.6 s |

合計はほぼ同じ（282 s vs 293 s）。ROCm ≥ 7.14 系では ComfyUI の DynamicVRAM が既定で有効になり gfx1151/Windows で
アクセス違反の報告があるため、`--highvram`（本リポジトリの既定）で無効化しておくこと。
