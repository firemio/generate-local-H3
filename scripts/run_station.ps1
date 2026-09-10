# 24/7 launcher for the AI TV station.
#   .\scripts\run_station.ps1            # start ComfyUI (if not running) + station (player :8800, HLS, production loop forever)
#   .\scripts\run_station.ps1 -Restart   # kill previous station processes first
#   .\scripts\run_station.ps1 -Stop      # stop everything
# Logs: output\comfyui.err.log, output\station.log
# To run at logon (optional): Task Scheduler -> "At log on" -> powershell -ExecutionPolicy Bypass -File <this script>
param(
    [switch]$Restart,
    [switch]$Stop,
    [int]$Port = 8800,
    [int]$Width = 832,
    [int]$Height = 480,
    [int]$Steps = 8,
    [string]$Formats = "",          # e.g. "news,cm,ident" to cycle; empty = weighted random schedule
    [string]$LlmModel = "gemma4:latest"
)
$ErrorActionPreference = "Continue"
$Root = Split-Path -Parent $PSScriptRoot
$Py = Join-Path $Root ".venv\Scripts\python.exe"
Set-Location $Root
New-Item -ItemType Directory -Force (Join-Path $Root "output") | Out-Null

function Stop-Station {
    Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -match '-m aitv' } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue; Write-Host "stopped aitv pid $($_.ProcessId)" }
    Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -match 'ffmpeg.*live\.m3u8' } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
}
if ($Stop) {
    Stop-Station
    Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -match 'ComfyUI\\main\.py' } |
        ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue; Write-Host "stopped ComfyUI pid $($_.ProcessId)" }
    return
}
if ($Restart) { Stop-Station; Start-Sleep -Seconds 2 }

# 1. ComfyUI (only if the API is not already answering)
$alive = $false
try { Invoke-RestMethod -Uri http://127.0.0.1:8188/system_stats -TimeoutSec 3 | Out-Null; $alive = $true } catch {}
if (-not $alive) {
    Write-Host "[station] starting ComfyUI"
    Start-Process -FilePath "powershell.exe" -ArgumentList "-NoProfile","-ExecutionPolicy","Bypass","-File",(Join-Path $Root "scripts\run_comfyui.ps1") `
        -RedirectStandardOutput (Join-Path $Root "output\comfyui.log") -RedirectStandardError (Join-Path $Root "output\comfyui.err.log") -WindowStyle Hidden | Out-Null
    for ($i = 0; $i -lt 90; $i++) {
        Start-Sleep -Seconds 2
        try { Invoke-RestMethod -Uri http://127.0.0.1:8188/system_stats -TimeoutSec 3 | Out-Null; $alive = $true; break } catch {}
    }
    if (-not $alive) { Write-Host "[station] ComfyUI did not come up; the producer will keep waiting for it" }
}

# 2. Ollama (writer LLM, CPU-pinned so the iGPU stays free for H3). Optional: falls back to built-in scripts.
try { Invoke-RestMethod -Uri http://127.0.0.1:11434/api/tags -TimeoutSec 3 | Out-Null; Write-Host "[station] Ollama OK ($LlmModel)" }
catch { Write-Host "[station] Ollama not reachable -> built-in fallback scripts will be used (start 'ollama serve' for LLM-written programmes)" }

# 3. Station: web player + HLS broadcaster + production loop (forever)
$args = @("-u", "-m", "aitv", "station", "--host", "127.0.0.1", "--port", "$Port", "--width", "$Width", "--height", "$Height", "--steps", "$Steps", "--llm-model", $LlmModel)
if ($Formats) { $args += @("--formats", $Formats) }
$env:PYTHONIOENCODING = "utf-8"
$env:AITV_LLM_NUM_GPU = "0"
$p = Start-Process -FilePath $Py -ArgumentList $args -RedirectStandardOutput (Join-Path $Root "output\station.log") `
        -RedirectStandardError (Join-Path $Root "output\station.err.log") -WindowStyle Hidden -PassThru
Write-Host "[station] started pid $($p.Id)  ->  http://127.0.0.1:$Port/   HLS: http://127.0.0.1:$Port/hls/live.m3u8"
Write-Host "[station] production is ~60x slower than real time on this GPU (a 6.6 s clip takes ~400 s at ${Width}x${Height});"
Write-Host "[station] the channel airs new programmes as soon as they are rendered and mixes the library in between."
