# 安装细节（需要手动逐步操作时）

本文说明 `scripts/setup.ps1` 到底做了什么。全部为 Windows 原生方式（不需要 WSL，不需要 HIP SDK）。

本页已按 **当前 `.venv` 的实测值**对齐（2026-10 时点。依据 ComfyUI 启动日志
`output/comfyui-boot*.log` 与 `pip` 的实际安装结果）。注意 `scripts/setup.ps1` 的默认参数
仍指向 ROCm 7.13 系列，**与本页不一致**（见 §6）。

## 前提

- 在 BIOS 中把分配给 iGPU 的 VRAM 开到最大（EVO-X2 128 GB 机型为 96 GB）。
  Windows 侧可见物理内存随之变为 ~32 GB（启动日志中为 `total RAM 32407 MB`）。
- AMD Software（Adrenalin）为 2026 年夏及以后的版本（已验证：32.0.31041.1004）。
- Git、Python 3.12（由 uv 安装）、ffmpeg（在 PATH 上）。

## 1. ROCm PyTorch（gfx1151 用 Windows wheel）

当前 `.venv` 是从 AMD 的 `whl-next` 索引以 `torch[device-gfx1151]` 形式安装的。
不需要单独安装 `rocm[libraries]`，作为 `torch` 的依赖会自动带入 `torch 2.11.0+rocm10.0.0` +
`rocm_sdk_core` / `rocm_sdk_libraries` / `rocm_sdk_device_gfx1151`（10.0.0）。

```powershell
uv venv .venv --python 3.12 --seed
.\.venv\Scripts\python.exe -m pip install --index-url https://stable.repo.amd.com/rocm/whl-next/ `
    "torch[device-gfx1151]==2.11.0+rocm10.0.0" "torchvision[device-gfx1151]==0.26.0+rocm10.0.0" "torchaudio==2.11.0.2+rocm10.0.0"
.\.venv\Scripts\python.exe scripts\check_gpu.py
```

`scripts\check_gpu.py` 的输出（在本 `.venv` 中实测）：

```
torch      : 2.11.0+rocm10.0.0
hip        : 7.15.26333
available  : True
device     : AMD Radeon(TM) 8060S Graphics gfx1151
```

`scripts\check_gpu.py` 随后还会跑 fp16/bf16 的 4096 矩阵乘和 SDPA 小基准。
`total mem` 看起来比物理 VRAM 分配更大，是因为 HIP 把共享部分也算进去了。实际可分配上限约 ~85 GB
（`docs/pitfalls.md` §2）。

其他选项（未使用）：
- `repo.amd.com/rocm/whl/gfx1151/`（旧索引）：最高到 torch 2.11.0+rocm7.13.0。该索引已在 2026-06 停止更新。
- `rocm.nightlies.amd.com/v2/gfx1151/`：开发版。torch 2.9.1+rocm7.13.0a2026042x。
- ROCm 7.2.1 官方 Windows PyTorch（repo.radeon.com）：torch 2.9.1，需要驱动 26.2.2，仅 cp312。

## 2. ComfyUI

```powershell
git clone --depth 1 --branch v0.35.0 https://github.com/comfyanonymous/ComfyUI.git ComfyUI
```

除去 requirements.txt 中的 `torch/torchvision/torchaudio` 三行后执行 pip install，最后再从上面的索引
重新安装 torch（确保 pip 没有把它换成 CUDA 版）。`comfy-kitchen==0.2.33` 有 Windows wheel，
在 ROCm 上 `hip` 后端可用（INT8 GEMM / sol_attn / adaln）。

把 `config/extra_model_paths.yaml` 复制到 `ComfyUI/` 后，ComfyUI 就会引用 `models/` 下的内容。

## 3. 模型

`scripts/download_models.ps1`（`huggingface_hub` + `hf_transfer`）。HF 上的路径是 `diffusion_models/…` 这样
直接在文件夹根下，所以写法是 `--include "vae/*"` 之类。

## 4. 启动

`scripts/run_comfyui.ps1`（前台一直运行，Ctrl+C 停止）。要点：

| 设置 | 理由 |
|---|---|
| `HIP_VISIBLE_DEVICES=0`，删除 `CUDA_VISIBLE_DEVICES` | 残留空字符串会让 HIP 设备被隐藏 |
| 将 `PYTORCH_HIP_ALLOC_CONF` 设为 **unset** | expandable_segments 在 Windows/HIP 上会在 14 GB 处 OOM（docs/pitfalls.md §1） |
| `--highvram` | 有 96 GB，故将 TE(26 GB)+DiT(20 GB)+VAE(5.5 GB) 常驻。在 ROCm ≥ 7.14 上同时会禁用 DynamicVRAM |
| `--bf16-unet` | gfx1151 支持 bf16 WMMA，且 H3 是按 bf16 训练的 |
| `--use-ck-attention` | 使用 comfy-kitchen 的 HIP INT8 attention（比 SDPA 快约 2.6 倍）。与 `--use-pytorch-cross-attention` 互斥 |
| `--disable-pinned-memory` | 有报告称在统一内存 APU 上 pinned 反而更慢 |
| `--reserve-vram 2` | 给桌面预留一点 |

启动后，在 `output/comfyui-boot*.log` 这类日志中（经 `run_station.ps1` 启动时为 `output/comfyui.log`）
**实际会出现**的行（摘自实测日志）：

```
[INFO] Found comfy_kitchen backend hip: {'available': True, 'disabled': False, ...
[INFO] Total VRAM 110457 MB, total RAM 32407 MB
[INFO] pytorch version: 2.11.0+rocm10.0.0
[INFO] AMD arch: gfx1151
[INFO] ROCm version: (7, 15)
[INFO] Set vram state to: HIGH_VRAM
[INFO] Using sub quadratic optimization for attention, if you have memory or speed issues try using: --use-split-cross-attention
[INFO] Using Comfy Kitchen attention
[INFO] ComfyUI version: 0.35.0
[INFO] comfy-kitchen version: 0.2.33
[INFO] To see the GUI go to: http://127.0.0.1:8188
```

以下两行值得注意，但**都属于正常现象**，可以放着不管。

- `Could not run flash attention, disabling it: CUDA error: invalid argument`（`hipErrorInvalidValue`）：
  gfx1151 上 aotriton flash attention 的启动探测失败了。由于 `--use-ck-attention` 会把后端切到
  Comfy Kitchen attention，所以没有实际影响。反过来，加 `--use-pytorch-cross-attention` 等于强制走
  这条失败的路径，因此不要加。
- `Using sub quadratic optimization for attention`：这是 PyTorch 默认后端在上面的失败之后给出的显示。
  紧随其后的 `Using Comfy Kitchen attention` 才表示实际使用的后端。

`Total VRAM` / `total RAM` 的数值随机器的内存配置而变（另一份启动日志中是 `102129 MB / 65175 MB`）。
`total RAM` 在 32 GB 左右说明 VRAM 划分较大。另外 `Total VRAM` 会显示得比物理分配更大
（分配 96 GiB 却显示 110457 MB），所以判断可用容量要看 `torch.cuda.mem_get_info()` 一侧。

## 5. 生成

默认为 `--mp 0.4`（相当于草稿档）、`--seconds 5`、`--steps 8`、`--seed 0`。显式写出来更稳妥：

```powershell
.\scripts\build.ps1     # Go 1.26+（winget install GoLang.Go），生成 bin\h3gen.exe 与 bin\aitv.exe
.\bin\h3gen.exe gen --prompt "..." --width 832 --height 480 --seconds 5 --steps 8
```

`h3gen`（Go，`cmd/h3gen` + `internal/comfy`）把 API 格式的图提交到 ComfyUI 的 `/prompt`，并用 WebSocket 的 `executing` 事件间隔
算出各节点耗时（与松尾先生的 profile_nodes.py 思路相同）。结果写入 `output/runs.jsonl`。
连接目标由 `--host`/`--port` 指定（默认 `127.0.0.1:8188`，环境变量 `COMFY_HOST`/`COMFY_PORT`）。

长时间、高分辨率生成时 DiT 被卸载会走进 `docs/pitfalls.md` §8 的崩溃路径，
因此有一个规避手段：用 `--clip-device cpu` 把文本编码器放到 CPU 上。

## 6. `setup.ps1` 的默认值（尚未更新）

`scripts/setup.ps1:12-13` 的默认值仍然如下：

```
$TorchIndex = "https://repo.amd.com/rocm/whl/gfx1151/"
$TorchSpec  = "torch==2.11.0+rocm7.13.0 torchvision==0.26.0+rocm7.13.0 torchaudio==2.11.0+rocm7.13.0"
```

也就是说与现行 `.venv`（ROCm 10.0.0）不是同一套。**全新搭建时**，要么给 `setup.ps1` 传 `-TorchIndex` / `-TorchSpec`，
要么先按 §1 的命令装好再用 `-SkipComfy`。**不要在已经建好的 `.venv` 上重跑 `setup.ps1`**
（它会把 torch 整套换掉）。

> 注：本页没有改动 `scripts/setup.ps1` 与 `scripts/run_comfyui.ps1` 的默认值和注释。
> 是否要把脚本侧也统一到 ROCm 10.0.0，另行判断。