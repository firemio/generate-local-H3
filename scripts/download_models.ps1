# Download the MiniMax H3 model files (Comfy-Org/MiniMax-H3) into .\models
# Requires the venv (scripts\setup.ps1) - uses huggingface_hub with hf_transfer.
#
#   .\scripts\download_models.ps1            # base set (fl2va int8 + int8 text encoder + VAEs + turbo LoRAs)  ~58 GB
#   .\scripts\download_models.ps1 -Ref2VA    # + ref2va int8 (reference / lipsync / anchor-consistency)        +21 GB
#   .\scripts\download_models.ps1 -Bf16      # + fl2va pruned bf16 (fallback if int8 kernels misbehave)        +40 GB
param(
    [switch]$Ref2VA,
    [switch]$Bf16,
    [switch]$Nvfp4TextEncoder
)
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
$Py = Join-Path $Root ".venv\Scripts\python.exe"
if (-not (Test-Path $Py)) { throw "venv not found: run scripts\setup.ps1 first" }
$Models = Join-Path $Root "models"
New-Item -ItemType Directory -Force $Models | Out-Null

$env:HF_HUB_ENABLE_HF_TRANSFER = "1"
$env:HF_HUB_DISABLE_PROGRESS_BARS = "0"

$files = @(
    "vae/minimax_h3_video_vae_fp16.safetensors",
    "vae/minimax_h3_audio_vae_fp32.safetensors",
    "loras/minimax_h3_fl2v_turbo_8step_v1.0_comfyui_bf16.safetensors",
    "loras/minimax_h3_fl2v_turbo_4step_v1.0_768p_comfyui_bf16.safetensors",
    "loras/minimax_h3_ref2v_turbo_4step_v0.1_comfyui_bf16.safetensors",
    "diffusion_models/minimax_h3_fl2va_pruned_int8_convrot.safetensors",
    "text_encoders/qwen3vl_32b_minimax_h3_int8_convrot.safetensors"
)
if ($Ref2VA) { $files += "diffusion_models/minimax_h3_ref2va_pruned_int8_convrot.safetensors" }
if ($Bf16)   { $files += "diffusion_models/minimax_h3_fl2va_pruned_bf16.safetensors" }
if ($Nvfp4TextEncoder) { $files += "text_encoders/qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors" }

$script = @'
import sys
from huggingface_hub import hf_hub_download
repo, local = sys.argv[1], sys.argv[2]
for f in sys.argv[3:]:
    print("[download]", f, flush=True)
    hf_hub_download(repo_id=repo, filename=f, local_dir=local)
print("[download] done")
'@
$tmp = Join-Path $env:TEMP "h3_download.py"
Set-Content -Path $tmp -Value $script -Encoding utf8
& $Py $tmp "Comfy-Org/MiniMax-H3" $Models @files
# embeddings (tiny, optional style embeddings)
& $Py -c "from huggingface_hub import snapshot_download; snapshot_download('Comfy-Org/MiniMax-H3', allow_patterns=['embeddings/*'], local_dir=r'$Models')"
Get-ChildItem -Recurse $Models -Filter *.safetensors | Select-Object @{n='GB';e={[math]::Round($_.Length/1GB,2)}}, FullName | Format-Table -AutoSize
