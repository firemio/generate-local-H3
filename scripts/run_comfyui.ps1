# Launch ComfyUI with settings tuned for AMD Strix Halo (Ryzen AI Max+ 395 / Radeon 8060S, gfx1151)
# on Windows with the ROCm PyTorch wheels from repo.amd.com.
#
# Usage:  .\scripts\run_comfyui.ps1 [-Port 8188] [-ExtraArgs "--verbose"]
param(
    [int]$Port = 8188,
    [string]$Listen = "127.0.0.1",
    [string]$ExtraArgs = "",
    [switch]$NoLaunchBrowser = $true
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
$Py = Join-Path $Root ".venv\Scripts\python.exe"
if (-not (Test-Path $Py)) { throw "venv not found: run scripts\setup.ps1 first" }

# --- ROCm / HIP environment -------------------------------------------------
# Only one GPU (the iGPU); keep HIP from probing anything else.
$env:HIP_VISIBLE_DEVICES = "0"
# A stray CUDA_VISIBLE_DEVICES="" hides the HIP device from torch ("No HIP GPUs available").
Remove-Item Env:CUDA_VISIBLE_DEVICES -ErrorAction SilentlyContinue
# Quiet HIP runtime logging.
$env:AMD_LOG_LEVEL = "0"
# Let the allocator use the whole 96 GB carve-out.
$env:GPU_MAX_HEAP_SIZE = "100"
$env:GPU_MAX_ALLOC_PERCENT = "100"
# !! Do NOT set PYTORCH_HIP_ALLOC_CONF=expandable_segments:True on Windows/HIP: with many small tensor
#    allocations (text encoder load) it hits "CUDA out of memory ... 94 GiB is free" at ~14 GB.
#    Verified with scripts\vram_probe3.py (2026-09-10). The default caching allocator reaches ~85 GB.
Remove-Item Env:PYTORCH_HIP_ALLOC_CONF -ErrorAction SilentlyContinue
Remove-Item Env:PYTORCH_CUDA_ALLOC_CONF -ErrorAction SilentlyContinue
# MIOpen: ComfyUI disables cudnn/MIOpen on AMD by default (faster). Set COMFYUI_ENABLE_MIOPEN=1 to re-enable.
# Model paths live in ..\models (see ComfyUI\extra_model_paths.yaml).

# --- ComfyUI flags -----------------------------------------------------------
# --highvram  : 96 GB of VRAM; keep models resident instead of paging to the 32 GB of system RAM.
# --bf16-unet : gfx1151 has bf16 WMMA; H3 is trained in bf16.
# --use-pytorch-cross-attention : SDPA (aotriton flash/mem-efficient) is auto-enabled for gfx1151 on torch>=2.7,
#                                 the flag forces it if the auto-probe declines.
# --disable-pinned-memory : pinned host memory is a known slowdown on unified-memory APUs.
# --reserve-vram 2 : leave a little for the desktop compositor.
$Args = @(
    (Join-Path $Root "ComfyUI\main.py"),
    "--listen", $Listen,
    "--port", "$Port",
    "--highvram",
    "--bf16-unet",
    "--use-pytorch-cross-attention",
    "--disable-pinned-memory",
    "--reserve-vram", "2",
    "--output-directory", (Join-Path $Root "output"),
    "--disable-auto-launch"
)
if ($ExtraArgs) { $Args += ($ExtraArgs -split " ") }

Write-Host "[run_comfyui] python: $Py"
Write-Host "[run_comfyui] args  : $($Args -join ' ')"
Set-Location (Join-Path $Root "ComfyUI")
& $Py @Args
