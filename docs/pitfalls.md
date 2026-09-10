# つまずきポイント（Strix Halo / Windows / ROCm で MiniMax H3）

実機（EVO-X2, 2026-09-10）で実際に踏んだものだけを記録。

## 1. `expandable_segments:True` は Windows HIP で使えない

症状: テキストエンコーダ（Qwen3-VL-32B int8, 27 GB）のロード中に

```
torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 126.00 MiB.
GPU 0 has a total capacity of 107.87 GiB of which 94.43 GiB is free.
Of the allocated memory 13.15 GiB is allocated by PyTorch
```

94 GB 空いているのに 13〜14 GB で落ちる。`scripts/vram_probe3.py` で環境変数を二分探索した結果、
`PYTORCH_HIP_ALLOC_CONF=expandable_segments:True` が原因（1 GB 単位の大きな確保なら 84 GB まで通るが、
数百個の小さな確保で ~14 GB で失敗する）。既定のキャッシングアロケータなら 27 GB 全量ロード可・上限 ~85 GB。

→ `run_comfyui.ps1` では明示的に unset している。

## 2. 1 プロセスの確保上限は約 85 GB

BIOS で 96 GB を VRAM に割り当てても、torch から実際に確保できるのは ~85 GB（`scripts/vram_probe.py`）。
int8 DiT (21 GB) + int8 TE (27 GB) + VAE (6 GB) = 54 GB は同時常駐できる（`--highvram`）。

## 3. `hf download --include "split_files/..."` は空振りする

Comfy-Org/MiniMax-H3 のパスは `diffusion_models/`, `text_encoders/`, `vae/`, `loras/` 直下。
`split_files/` プレフィックスは無い（他の Comfy-Org リポジトリと違う）。

## 4. pip が torch を CUDA 版に置き換える問題

`pip install -r ComfyUI/requirements.txt` は `torch` が未ピンなので、ROCm 版が入っていれば触らない。
ただし他パッケージが `torch>=X` を要求してアップグレードを試みると PyPI の CUDA/CPU 版に置き換わる。
`setup.ps1` は torch 3 点を除いた requirements を入れた後、AMD index から再インストールして担保している。

## 5. nvfp4_awq テキストエンコーダは NVIDIA 専用

公式テンプレートの既定は `qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors`（15.7 GB）だが、
comfy-kitchen の `hip` バックエンドには `dequantize_nvfp4` / `scaled_mm_nvfp4` が無い（eager フォールバックのみ）。
AMD では `int8_convrot` 版（27 GB）を使う。HF discussion Comfy-Org/MiniMax-H3 #33 でも同じ結論。

## 6. Triton バックエンドは Windows ROCm には無い

comfy-kitchen の INT8 Triton カーネルは使えない（`No module named 'triton'`）。
代わりに `hip` バックエンド（`int8_linear`, `sol_attn`, `adaln` 等）が gfx1151 で有効。

## 7. gh CLI は winget / choco が使えない環境がある

非管理者・winget がハングする環境だったため、GitHub releases の zip を `.tools/gh/` に展開して使用。
GCM に保存済みトークンは `read:org` スコープが無く `gh auth login --with-token` に拒否されるので、
`GH_TOKEN` 環境変数で直接渡す。

## 8. `/free`（モデルのアンロード）で access violation（ROCm 10.0 スタック）

ROCm 10.0.0 + torch 2.13 + comfy-kitchen 0.2.33 で、INT8(convrot) の DiT を常駐させた状態で
`POST /free {"unload_models": true}` を呼ぶと、`comfy_kitchen/tensor/base.py: _handle_to`（量子化テンソルの
GPU→CPU 移動）で `Windows fatal exception: access violation` となりサーバごと落ちた（2026-09-10）。
comfy-kitchen の Windows 用 HIP `.pyd` は別の ROCm SDK でビルドされており、10.0 との組み合わせは未検証扱い。

→ 対策: 量子化モデルをアンロードしない運用にする。fl2va と ref2va を切り替える場合はサーバを再起動する
（`--highvram` のため自動アンロードも起きにくいが、85 GB 上限を超えると OOM→アンロードが走る）。
既定の 7.13 スタックではこの経路は未検証。
