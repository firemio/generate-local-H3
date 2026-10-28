# 踩坑记录（Strix Halo / Windows / ROCm 上跑 MiniMax H3）

只记录在实机（EVO-X2，2026-09-10）上真正踩到的。

## 1. `expandable_segments:True` 在 Windows HIP 上不能用

症状：加载文本编码器（Qwen3-VL-32B int8, 27 GB）时出现

```
torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 126.00 MiB.
GPU 0 has a total capacity of 107.87 GiB of which 94.43 GiB is free.
Of the allocated memory 13.15 GiB is allocated by PyTorch
```

还剩 94 GB 空闲，却在 13〜14 GB 处挂掉。用 `scripts/vram_probe3.py` 对环境变量做二分查找后，
确认原因是 `PYTORCH_HIP_ALLOC_CONF=expandable_segments:True`（以 1 GB 为单位的大块分配能到 84 GB，
但成百上千次小块分配会在 ~14 GB 处失败）。换成默认的 caching allocator 就能完整加载 27 GB，上限 ~85 GB。

→ `run_comfyui.ps1` 中显式 unset 了它。

## 2. 单进程可分配上限约 85 GB

即使在 BIOS 中划分了 96 GB 给 VRAM，从 torch 实际能分配的也只有 ~85 GB（`scripts/vram_probe.py`）。
int8 DiT (21 GB) + int8 TE (27 GB) + VAE (6 GB) = 54 GB 可以同时常驻（`--highvram`）。

## 3. `hf download --include "split_files/..."` 会空跑

Comfy-Org/MiniMax-H3 的路径直接位于 `diffusion_models/`, `text_encoders/`, `vae/`, `loras/` 之下。
没有 `split_files/` 前缀（这点与其他 Comfy-Org 仓库不同）。

## 4. pip 把 torch 换成 CUDA 版的问题

`pip install -r ComfyUI/requirements.txt` 中 `torch` 未锁定版本，如果已经装了 ROCm 版就不会动它。
但只要其他包要求 `torch>=X` 而尝试升级，就会被替换成 PyPI 上的 CUDA/CPU 版。
`setup.ps1` 的做法是：先安装去掉 torch 三件套的 requirements，再从 AMD 索引重装一遍来保证。

## 5. nvfp4_awq 文本编码器是 NVIDIA 专用的

官方模板默认用 `qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors`（15.7 GB），但
comfy-kitchen 的 `hip` 后端没有 `dequantize_nvfp4` / `scaled_mm_nvfp4`（只有 eager 回退）。
在 AMD 上使用 `int8_convrot` 版（27 GB）。HF discussion Comfy-Org/MiniMax-H3 #33 也是同样结论。

## 6. Triton 后端在 Windows ROCm 上没有

comfy-kitchen 的 INT8 Triton 内核用不了（`No module named 'triton'`）。
替代的是 `hip` 后端（`int8_linear`, `sol_attn`, `adaln` 等）在 gfx1151 上可用。

## 7. gh CLI 在部分环境里 winget / choco 不可用

遇到过非管理员、winget 会卡住的环境，于是把 GitHub releases 的 zip 解压到 `.tools/gh/` 后使用。
GCM 里保存的令牌没有 `read:org` 作用域，会被 `gh auth login --with-token` 拒绝，
因此改为通过 `GH_TOKEN` 环境变量直接传入。

## 8. `/free`（卸载模型）触发 access violation（ROCm 10.0 栈）

在 ROCm 10.0.0 + torch 2.13 + comfy-kitchen 0.2.33 下，当 INT8(convrot) 的 DiT 处于常驻状态时
调用 `POST /free {"unload_models": true}`，会在 `comfy_kitchen/tensor/base.py: _handle_to`（量化张量的
GPU→CPU 搬运）处出现 `Windows fatal exception: access violation`，整个服务端随之崩溃（2026-09-10）。
comfy-kitchen 的 Windows 版 HIP `.pyd` 是用另一个 ROCm SDK 构建的，与 10.0 的组合按未验证对待。

→ 对策：采用不卸载量化模型的运行方式。需要从 fl2va 切到 ref2va 时，重启服务端
（因为 `--highvram` 自动卸载本来就很少发生，但一旦超过 85 GB 上限就会触发 OOM→卸载）。
默认的 7.13 栈下这条路径尚未验证。

## 9. 应用 turbo LoRA 时 OOM → 卸载导致 access violation（默认的 7.13 栈）

2026-09-14，服务端刚启动后的第一次生成（`h3gen gen`，t2v, turbo=True）在 `lora` 节点的权重合并过程中挂掉。
`comfy/ops.py: set_weight` → `comfy_kitchen ... quantize_int8_convrot_weight` 处出现

```
torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 222.00 MiB.
GPU 0 has a total capacity of 107.87 GiB of which 57.83 GiB is free.
Of the allocated memory 49.01 GiB is allocated by PyTorch
```

只用了 50 GB（上限 ~85 GB，`check_gpu.py` 显示 115 GB 空闲）却分配失败，随后 OOM 恢复流程的
`unload_all_models` → `unpatch_model` 以与 §8 相同的 access violation 让整个服务端崩溃。h3gen 一侧只会看到
`websocket closed: [WinError 10054]`，所以不看 **ComfyUI 的日志**就查不出原因。
把分辨率从 832x480 降到 640x352 也一样在同一处失败（LoRA 合并是模型固定尺寸的处理，与分辨率无关）。
到 9/12 为止同样配置下电视台还能正常运行，所以可能是重启后有什么变了，但尚未确定。

→ 对策：`--no-turbo`（不用 LoRA，8 步。640x352x124f 为 209 s，第二部起 140 s）。
`.\bin\aitv.exe station ... --no-turbo` / `run_station.ps1 -NoTurbo` 同理。

## 10. §9 的真正原因是「提交限额」（页面文件）— 2026-10-13 查明

§9 中「明明只用了 50 GB 却挂掉」的问题已经解决。原因**不是 GPU 显存，而是 Windows 的
提交限额（commit limit，即最大可提交量）**。这台机器把 96 GB 划分给了 GPU 的 VRAM，
因此 **Windows 可见的物理 RAM 只有 31.6 GB**。

```
提交限额       = 物理 RAM 31.6 GB + 页面文件大小
ComfyUI 峰值提交 = 61.10 GB   （用 PeakPagedMemorySize64 跑完一整轮测得）
页面文件实际使用峰值 = 约 10 GB 而已
```

第 3 行很重要：**这 61 GB 几乎全是物理 RAM，页面文件几乎没有被碰过**。
需要的并不是「让页面文件真的消耗掉 61 GB」，而是 **Windows 能够对 61 GB 的提交量
「做出承诺」**。这与实际是否发生分页无关。

页面文件由系统管理，Windows 会根据近期需求自动调整大小。测得的值：

| 时点 | 提交限额 | 页面文件 | 结果 |
|---|---|---|---|
| 刚启动（缩小的状态） | 40.9 GB | 9.25 GB | **应用 turbo LoRA 时 OOM → access violation** |
| 运行中（自动扩展后） | 108.2 GB | 75.10 GB | 正常跑完 |

也就是说 **刚启动时页面文件还很小就会挂掉**。这也解释了为什么「9/12 之前还能跑」，
某天却突然挂掉——取决于当时的页面文件大小。

→ 对策A（永久，需要管理员 + 重启）：给页面文件设置下限，使其不会缩小。

```powershell
# 预览（不做任何修改）
.\scripts\fix_pagefile.ps1 -WhatIfOnly
# 应用（需要 UAC 提权 + 重启）
.\scripts\fix_pagefile.ps1
```

→ 对策B（永久，应用后确认）：用 `.\scripts\check_commit.ps1` 以 GB 为单位显示缺口量。

→ 对策C（立即，无需重启）：**只重启 ComfyUI 也有效**。长时间运行的实例会囤积提交量，
实测重启后可用提交量从 17.4 GB 恢复到 55 GB。
想用 turbo 时，先重启 ComfyUI 再试。

注：如 §1 所述，不要与 `PYTORCH_HIP_ALLOC_CONF=expandable_segments:True` 同时使用。

### 诊断技巧

h3gen 一侧只会看到 `websocket closed: [WinError 10054]`。**务必去看 ComfyUI 一侧的日志**：

```
CUDA out of memory. Tried to allocate 148.00 MiB.
GPU 0 has a total capacity of 107.87 GiB of which 44.82 GiB is free.
```

「还有 44 GB 空闲却分配不出 148 MB」→ 这就是提交限额的信号。
另外 §9 的 access violation 是 OOM 恢复处理 `unload_all_models` → `unpatch_model` 的副作用，
要注意**真正杀死进程的是恢复路径，而不是 OOM 本身**。