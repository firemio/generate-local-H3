# generate-local-H3

**MiniMax H3（オープンウェイト 33B 動画+音声生成モデル）を GMKtec EVO-X2（AMD Ryzen AI Max+ 395 / Radeon 8060S, gfx1151, Windows 11）でローカル実行する**ための再現可能なセットアップと、ヘッドレス生成クライアント、AIテレビ局パイプラインです。

参考: [テクノエッジ 2026/09/06 松尾公也氏「MiniMax H3 高速化」記事](https://www.techno-edge.net/article/2026/09/06/5468.html) と [matsuo-koya/minimax-h3-notes](https://github.com/matsuo-koya/minimax-h3-notes)（RTX 5090 環境）。本リポジトリはそれを **NVIDIA 無し・AMD Strix Halo・Windows** で成立させたものです。

## 検証環境

| 項目 | 値 |
|---|---|
| PC | GMKtec NucBox EVO-X2 |
| CPU/GPU | AMD Ryzen AI MAX+ 395 / Radeon 8060S (gfx1151, RDNA 3.5, 40CU) |
| メモリ | 128 GB LPDDR5x（BIOS で 96 GB を VRAM に割当、OS 側 32 GB） |
| OS | Windows 11 Pro 26200 |
| GPU ドライバ | AMD 32.0.31041.1004 (2026-08) |
| Python | 3.12 (uv) |
| PyTorch | 2.11.0+rocm7.13.0（AMD 公式 gfx1151 wheel, `repo.amd.com/rocm/whl/gfx1151/`） |
| ComfyUI | v0.35.0 + comfy-kitchen 0.2.33（HIP バックエンドで INT8 が動く） |

## クイックスタート

```powershell
git clone https://github.com/firemio/generate-local-H3
cd generate-local-H3
.\scripts\setup.ps1              # venv + ROCm torch + ComfyUI + 依存（モデル以外）
.\scripts\download_models.ps1    # 約 58 GB（HF: Comfy-Org/MiniMax-H3）
.\scripts\run_comfyui.ps1        # http://127.0.0.1:8188
```

別ターミナルで:

```powershell
.\.venv\Scripts\python.exe -m h3gen gen --prompt "A golden retriever runs along a sunny beach toward the camera. Audio: waves, wind." --seconds 4 --steps 8
```

出力は `output/h3/*.mp4`（H.264 + 32 kHz ステレオ音声、24 fps）。各実行の所要時間・ノード別プロファイルは `output/runs.jsonl` に追記されます。

## 構成

```
scripts/setup.ps1            再現用ワンショットセットアップ
scripts/download_models.ps1  モデル取得（int8 pruned + int8 TE + VAE + turbo LoRA）
scripts/run_comfyui.ps1      Strix Halo 向けフラグ/環境変数で ComfyUI を起動
scripts/check_gpu.py         ROCm torch の動作確認とミニベンチ
config/extra_model_paths.yaml  models/ を ComfyUI から参照
h3gen/                       ヘッドレス生成クライアント（API グラフ生成・WebSocket プロファイラ・CLI）
aitv/                        AIテレビ局: ローカルLLM で番組表→H3 で生成→HLS 連続配信
workflows/templates/         ComfyUI 公式 H3 テンプレート（参照用）
docs/                        セットアップ詳細、つまずきポイント、計測ログ
report/                      HTML レポート
```

## モデル選択（96 GB VRAM の AMD 向け）

| 役割 | ファイル | サイズ | 備考 |
|---|---|---|---|
| DiT (T2V/I2V) | `minimax_h3_fl2va_pruned_int8_convrot.safetensors` | 21 GB | comfy-kitchen の **hip** バックエンドで INT8 GEMM が動作 |
| DiT (参照/リップシンク) | `minimax_h3_ref2va_pruned_int8_convrot.safetensors` | 21 GB | `download_models.ps1 -Ref2VA` |
| テキストエンコーダ | `qwen3vl_32b_minimax_h3_int8_convrot.safetensors` | 27 GB | nvfp4_awq 版は NVIDIA 向けなので使わない |
| VAE | `minimax_h3_video_vae_fp16` / `minimax_h3_audio_vae_fp32` | 5.8 GB | |
| Turbo LoRA | fl2v 8step / 4step, ref2v 4step | 各 2 GB | 20 step → 8 step |

## 実測（このPC、8-step turbo LoRA、INT8 DiT + INT8 テキストエンコーダ）

| 解像度 × 長さ | attention | サンプリング | VAE デコード | 合計（実行時間） |
|---|---|---|---|---|
| 832×480 × 124f (5.2s) | pytorch SDPA (aotriton) | 571 s (71.4 s/step) | 59 s | 636 s |
| 832×480 × 124f (5.2s) | **comfy kitchen INT8 (hip)** | **223 s (27.8 s/step)** | 59 s | **285 s** |
| 832×480 × 158f (6.6s) 日本語セリフ | INT8 | 319 s (39.9 s/step) | 76 s | 403 s |
| 640×352 × 124f (5.2s) | INT8 | 108 s (13.4 s/step) | 29 s | 142 s |

- 初回のみモデルロード（TE 26 GB + DiT 20 GB + VAE 5.5 GB）で +60〜90 s。以後は常駐（`--highvram`）。
- ピーク VRAM 約 78 GB（832×480×124f）。1 プロセスの確保上限 ~85 GB。
- ROCm 10.0.0（torch 2.13）でも動作: サンプリング ~10% 遅く、VAE デコード ~16% 速い（`docs/setup.md`）。

## 動作原理（要点）

- **ROCm on Windows**: AMD が `repo.amd.com/rocm/whl/gfx1151/` で配布する PyTorch 2.11 + ROCm 7.13 の Windows wheel を使用。WSL も HIP SDK も不要。`torch.cuda.is_available()` が True になり、HIP デバイスとして 8060S が見える。
- **ComfyUI フラグ**: `--highvram --bf16-unet --use-ck-attention --disable-pinned-memory --reserve-vram 2`。INT8 アテンション（comfy-kitchen hip）はサンプリングを 2.6 倍高速化。外すと gfx1151 では aotriton の SDPA が自動で有効になる。
- **INT8**: ComfyUI 0.35 の comfy-kitchen 0.2.33 は `hip` バックエンドを持ち、`int8_linear` / `dequantize_int8_convrot_weight_dtype` / `sol_attn` が gfx1151 で使える（Triton 不要）。
- **グラフ**: 公式テンプレートと同一（UNETLoader → LoraLoaderModelOnly → BasicGuider/BasicScheduler(res_multistep, simple) → SamplerCustomAdvanced → VAEDecode + VAEDecodeAudio → CreateVideo(24fps) → SaveVideo）。`h3gen/graph.py` が API 形式で組み立てる。

詳細・計測値・落とし穴は `docs/` と `report/` を参照。

## ライセンス

このリポジトリのコード（`scripts/`, `h3gen/`, `aitv/`, `config/`, `docs/`）は MIT です（[LICENSE](LICENSE)）。

同梱の ComfyUI 公式テンプレート（`workflows/templates/`, MIT, Comfy Org）と、MiniMax H3 で生成した
動画・静止画（`input/`, `report/assets/`）は別条件です。生成物はモデル提供元の
MiniMax H3 Community License Agreement に従います。詳細は [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。

ComfyUI 本体（GPL-3.0）とモデルの重みはセットアップ時に取得するもので、このリポジトリには含まれません。
