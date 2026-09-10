# One-shot reproducible setup for MiniMax H3 on AMD Strix Halo (Windows 11, ROCm PyTorch)
#
#   git clone https://github.com/firemio/generate-local-H3 ; cd generate-local-H3
#   .\scripts\setup.ps1              # venv + ROCm torch + ComfyUI + deps (no models)
#   .\scripts\download_models.ps1    # ~58 GB
#   .\scripts\run_comfyui.ps1        # http://127.0.0.1:8188
#
# Tested: GMKtec EVO-X2, Ryzen AI Max+ 395, Radeon 8060S (gfx1151), 128 GB (96 GB VRAM carve-out in BIOS),
#         Windows 11 Pro 26200, AMD driver 32.0.31041.1004, Python 3.12, uv 0.9.
param(
    [string]$ComfyTag = "v0.35.0",
    [string]$TorchIndex = "https://repo.amd.com/rocm/whl/gfx1151/",
    [string]$TorchSpec = "torch==2.11.0+rocm7.13.0 torchvision==0.26.0+rocm7.13.0 torchaudio==2.11.0+rocm7.13.0",
    [switch]$SkipComfy
)
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
Write-Host "== generate-local-H3 setup in $Root"

# 0. uv (python manager)
if (-not (Get-Command uv -ErrorAction SilentlyContinue)) {
    Write-Host "== installing uv"
    powershell -ExecutionPolicy ByPass -c "irm https://astral.sh/uv/install.ps1 | iex"
    $env:PATH = "$env:USERPROFILE\.local\bin;$env:PATH"
}

# 1. venv (Python 3.12: the AMD wheels ship cp312/cp313; ComfyUI is happiest on 3.12)
if (-not (Test-Path ".venv\Scripts\python.exe")) {
    Write-Host "== creating .venv (python 3.12)"
    uv venv .venv --python 3.12 --seed
}
$Py = Join-Path $Root ".venv\Scripts\python.exe"
& $Py -m pip install --upgrade pip | Out-Null

# 2. ROCm PyTorch for gfx1151 (AMD's index; pulls rocm-sdk-core / rocm-sdk-libraries-gfx1151 with it)
Write-Host "== installing ROCm torch from $TorchIndex"
& $Py -m pip install --index-url $TorchIndex ($TorchSpec -split " ")
& $Py (Join-Path $Root "scripts\check_gpu.py")

if (-not $SkipComfy) {
    # 3. ComfyUI at a pinned tag (H3 nodes need >= 0.30; ModelAttentionBackend >= 0.33)
    if (-not (Test-Path "ComfyUI\main.py")) {
        Write-Host "== cloning ComfyUI $ComfyTag"
        git clone --depth 1 --branch $ComfyTag https://github.com/comfyanonymous/ComfyUI.git ComfyUI
    }
    # 4. ComfyUI deps. torch is already satisfied by the ROCm build, so pip must NOT touch it:
    #    install everything except the torch trio, then re-assert the ROCm wheels.
    Write-Host "== installing ComfyUI requirements"
    $req = Get-Content "ComfyUI\requirements.txt" | Where-Object { $_ -notmatch '^\s*(torch|torchvision|torchaudio)\s*$' }
    $reqTmp = Join-Path $env:TEMP "comfy-req-notorch.txt"
    Set-Content -Path $reqTmp -Value $req -Encoding utf8
    & $Py -m pip install -r $reqTmp
    & $Py -m pip install --index-url $TorchIndex ($TorchSpec -split " ")   # no-op if intact, repairs if clobbered
    # 5. our own client deps
    & $Py -m pip install websocket-client "huggingface_hub[hf_transfer]" requests
    # 6. models live in ..\models (kept out of the ComfyUI checkout)
    Copy-Item (Join-Path $Root "config\extra_model_paths.yaml") (Join-Path $Root "ComfyUI\extra_model_paths.yaml") -Force
    New-Item -ItemType Directory -Force "models\diffusion_models","models\text_encoders","models\vae","models\loras","models\embeddings","output" | Out-Null
}

& $Py -c "import torch; assert torch.cuda.is_available(), 'HIP device not visible'; print('torch', torch.__version__, torch.cuda.get_device_name(0))"
Write-Host "== setup done. Next: .\scripts\download_models.ps1  then  .\scripts\run_comfyui.ps1"
