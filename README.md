# generate-local-H3

**在 GMKtec EVO-X2（AMD Ryzen AI Max+ 395 / Radeon 8060S, gfx1151, Windows 11）上本地运行 MiniMax H3（开源 33B 视频+音频生成模型）**的可复现安装流程，以及无头生成客户端和 AI 电视台管线。

参考：[TechNode 2026/09/06 松尾公也《MiniMax H3 提速》文章](https://www.techno-edge.net/article/2026/09/06/5468.html) 与 [matsuo-koya/minimax-h3-notes](https://github.com/matsuo-koya/minimax-h3-notes)（RTX 5090 环境）。本仓库是在 **没有 NVIDIA、使用 AMD Strix Halo、Windows** 的条件下把它跑通的版本。

## 验证环境

| 项目 | 值 |
|---|---|
| 机型 | GMKtec NucBox EVO-X2 |
| CPU/GPU | AMD Ryzen AI MAX+ 395 / Radeon 8060S (gfx1151, RDNA 3.5, 40CU) |
| 内存 | 128 GB LPDDR5x（BIOS 中划 96 GB 给 VRAM，OS 侧剩 32 GB） |
| 操作系统 | Windows 11 Pro 26200 |
| GPU 驱动 | AMD 32.0.31041.1004 (2026-08) |
| Python | 3.12 (uv) |
| PyTorch | 2.11.0+rocm10.0.0（HIP 7.15.26333，来自 `stable.repo.amd.com/rocm/whl-next/` 的 `torch[device-gfx1151]`） |
| ComfyUI | v0.35.0 + comfy-kitchen 0.2.33（HIP 后端下 INT8 可用） |
| 关于实测值 | 「实测」表中的数字是 ROCm 7.13 时代（torch 2.11 + ROCm 7.13）的测量结果。当前 `.venv` 是 ROCm 10.0.0 |

## 快速开始

```powershell
git clone https://github.com/firemio/generate-local-H3
cd generate-local-H3
.\scripts\setup.ps1              # venv + ROCm torch + ComfyUI + 依赖（不含模型）
.\scripts\download_models.ps1    # 约 58 GB（HF: Comfy-Org/MiniMax-H3）
.\scripts\run_comfyui.ps1        # http://127.0.0.1:8188（前台运行，Ctrl+C 停止）
```

`setup.ps1` 的默认值仍指向 ROCm 7.13 系列（`repo.amd.com/rocm/whl/gfx1151/`）。当前 `.venv` 已是
ROCm 10.0.0 构建，所以不要在已有环境上重跑 `setup.ps1`（会把 torch 换掉）。详见 `docs/setup.md`。

在另一个终端（生成客户端和电视台是 Go 写的：装好 Go 1.26+ 后先构建一次）：

```powershell
.\scripts\build.ps1              # → bin\h3gen.exe, bin\aitv.exe
.\bin\h3gen.exe gen --prompt "A golden retriever runs along a sunny beach toward the camera. Audio: waves, wind." --seconds 4 --steps 8
```

输出在 `output/h3/*.mp4`（H.264 + 32 kHz 立体声，24 fps）。每次运行的耗时与逐节点剖析会追加写入 `output/runs.jsonl`。

## 结构

```
scripts/setup.ps1            可复现的一次性安装
scripts/download_models.ps1  模型下载（int8 pruned + int8 TE + VAE + turbo LoRA）
scripts/run_comfyui.ps1      以 Strix Halo 专用参数/环境变量启动 ComfyUI
scripts/build.ps1            构建 Go 二进制（bin\h3gen.exe, bin\aitv.exe）
scripts/run_station.ps1      电视台的 24 小时常驻启动器
scripts/check_gpu.py         ROCm torch 的运行检查与小型基准
config/extra_model_paths.yaml  让 ComfyUI 引用 models/ 目录
cmd/h3gen/                   无头生成 CLI（Go）：API 图构建 · WebSocket 剖析
cmd/aitv/, internal/         AI 电视台（Go）：本地 LLM 出节目单 → H3 生成 → HLS 连续播出
aitv/                        局的数据（library/ 生成好的片段与台本，hls/ 播出窗口，web/ 播放器）
archive/python/              最初的 Python 实现（供参照，已不再运行）
workflows/templates/         ComfyUI 官方 H3 模板（供参照）
docs/                        安装细节、踩坑记录、测量日志
report/                      HTML 报告
```

## 模型选择（面向 96 GB VRAM 的 AMD）

| 角色 | 文件 | 大小 | 备注 |
|---|---|---|---|
| DiT (T2V/I2V) | `minimax_h3_fl2va_pruned_int8_convrot.safetensors` | 21 GB | comfy-kitchen 的 **hip** 后端可跑 INT8 GEMM |
| DiT (参考/对口型) | `minimax_h3_ref2va_pruned_int8_convrot.safetensors` | 21 GB | `download_models.ps1 -Ref2VA` |
| 文本编码器 | `qwen3vl_32b_minimax_h3_int8_convrot.safetensors` | 27 GB | nvfp4_awq 版是面向 NVIDIA 的，不要用 |
| VAE | `minimax_h3_video_vae_fp16` / `minimax_h3_audio_vae_fp32` | 5.8 GB | |
| Turbo LoRA | fl2v 8step / 4step, ref2v 4step | 各 2 GB | 20 step → 8 step |

## 实测（本机，8-step turbo LoRA，INT8 DiT + INT8 文本编码器）

| 分辨率 × 长度 | attention | 采样 | VAE 解码 | 合计（执行时间） |
|---|---|---|---|---|
| 832×480 × 124f (5.2s) | pytorch SDPA (aotriton) | 571 s (71.4 s/step) | 59 s | 636 s |
| 832×480 × 124f (5.2s) | **comfy kitchen INT8 (hip)** | **223 s (27.8 s/step)** | 59 s | **285 s** |
| 832×480 × 158f (6.6s) 日语句 | INT8 | 319 s (39.9 s/step) | 76 s | 403 s |
| 640×352 × 124f (5.2s) | INT8 | 108 s (13.4 s/step) | 29 s | 142 s |

- 只有首次需要加载模型（TE 26 GB + DiT 20 GB + VAE 5.5 GB），多花 +60〜90 s。之后常驻（`--highvram`）。
- 峰值 VRAM 约 78 GB（832×480×124f）。单进程可分配上限 ~85 GB。
- ROCm 10.0.0（torch 2.13）下同样可用：采样慢 ~10%，VAE 解码快 ~16%（`docs/setup.md`）。

## 工作原理（要点）

- **Windows 上的 ROCm**：使用 AMD 在 `stable.repo.amd.com/rocm/whl-next/` 分发的 PyTorch（`torch[device-gfx1151]`）。不需要 WSL，也不需要 HIP SDK。`torch.cuda.is_available()` 返回 True，8060S 以 HIP 设备的形式可见。当前环境是 torch 2.11.0+rocm10.0.0（HIP 7.15.26333，ComfyUI 启动日志中显示为 `ROCm version: (7, 15)`）。
- **ComfyUI 参数**：`--highvram --bf16-unet --use-ck-attention --disable-pinned-memory --reserve-vram 2`。INT8 attention（comfy-kitchen hip）把采样提速 2.6 倍。不加这个参数时，本机上 aotriton 的 flash attention 初始化会失败（`Could not run flash attention ... hipErrorInvalidValue`），PyTorch 侧回退到 sub-quadratic（`docs/setup.md` §4）。
- **INT8**：ComfyUI 0.35 自带的 comfy-kitchen 0.2.33 有 `hip` 后端，`int8_linear` / `dequantize_int8_convrot_weight_dtype` / `sol_attn` 在 gfx1151 上可用（不需要 Triton）。
- **图**：与官方 t2v 模板的骨架相同（UNETLoader → LoraLoaderModelOnly → BasicGuider/BasicScheduler(res_multistep, simple) → SamplerCustomAdvanced → VAEDecode + VAEDecodeAudio → CreateVideo(24fps) → SaveVideo）。其中 turbo LoRA 默认会加，`MiniMaxH3SigmaShift` 与 `ModelAttentionBackend` 只在传了对应参数时才出现在图里。由 `internal/comfy/graph.go` 以 API 格式组装（Python 版 `h3gen/graph.py` 见 `archive/python/`）。

细节、测量值、踩坑记录请见 `docs/` 和 `report/`。

## 许可证

本仓库的代码（`cmd/`, `internal/`, `scripts/`, `archive/`, `config/`, `docs/`, `aitv/web/index.html`）采用 MIT 许可（[LICENSE](LICENSE)）。

随附的 ComfyUI 官方模板（`workflows/templates/`, MIT, Comfy Org）以及用 MiniMax H3 生成的
视频·图片（`input/`, `report/assets/`）适用不同条件。生成物遵循模型提供方的
MiniMax H3 Community License Agreement。详见 [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md)。

ComfyUI 本体（GPL-3.0）与模型权重属于安装时获取的内容，不包含在本仓库中。
