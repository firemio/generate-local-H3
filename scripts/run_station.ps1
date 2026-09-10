# 24/7 launcher for the AI TV station.
#   .\scripts\run_station.ps1            # start ComfyUI (if needed) + station (player :8800, HLS, production loop)
#   .\scripts\run_station.ps1 -Restart   # stop a running station first
#   .\scripts\run_station.ps1 -Stop      # stop the station (and ComfyUI with -All)
#   .\scripts\run_station.ps1 -Status    # what is running
# Logs: output\station.log, output\station.err.log, output\comfyui.err.log
# At logon (optional): Task Scheduler -> "At log on" -> powershell -ExecutionPolicy Bypass -File <this script>
param(
    [switch]$Restart,
    [switch]$Stop,
    [switch]$Status,
    [switch]$All,                   # with -Stop: also stop ComfyUI
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
$PidFile = Join-Path $Root "output\station.pid"
Set-Location $Root
New-Item -ItemType Directory -Force (Join-Path $Root "output") | Out-Null

function Get-StationPid {
    # The recorded PID is authoritative. Never match on a command-line substring: '-m aitv' also
    # appears in unrelated shells' command lines and killing those takes down the user's own tools.
    if (-not (Test-Path $PidFile)) { return $null }
    $recorded = (Get-Content $PidFile -ErrorAction SilentlyContinue | Select-Object -First 1)
    if (-not $recorded) { return $null }
    $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $recorded" -ErrorAction SilentlyContinue
    if ($proc -and $proc.CommandLine -match 'aitv') { return [int]$recorded }
    return $null
}
function Stop-Station {
    $stationPid = Get-StationPid
    if ($stationPid) {
        # /T kills the tree, so the station's ffmpeg children (HLS encoder, remux, normalize) go too.
        & taskkill.exe /PID $stationPid /T /F 2>&1 | Out-Null
        Write-Host "[station] stopped pid $stationPid (and its ffmpeg children)"
    } else {
        Write-Host "[station] no running station recorded in output\station.pid"
    }
    Remove-Item $PidFile -ErrorAction SilentlyContinue
}
function Test-Comfy {
    try { Invoke-RestMethod -Uri http://127.0.0.1:8188/system_stats -TimeoutSec 3 | Out-Null; return $true } catch { return $false }
}

if ($Status) {
    $stationPid = Get-StationPid
    if ($stationPid) { Write-Host "[station] running, pid $stationPid  ->  http://127.0.0.1:$Port/" }
    else { Write-Host "[station] not running" }
    Write-Host ("[station] ComfyUI: " + $(if (Test-Comfy) { "up" } else { "down" }))
    $lib = Join-Path $Root "aitv\library"
    if (Test-Path $lib) { Write-Host "[station] library: $((Get-ChildItem $lib -Filter *.mp4).Count) clips" }
    return
}
if ($Stop) {
    Stop-Station
    if ($All) {
        Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -match 'ComfyUI\\main\.py' } |
            ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue; Write-Host "[station] stopped ComfyUI pid $($_.ProcessId)" }
    }
    return
}
if ($Restart) { Stop-Station; Start-Sleep -Seconds 3 }

# Single instance: a second station would run a second producer and a second HLS encoder over the
# same library. (server.py also refuses to rebind :8800 on Windows, but fail here with a clear message.)
$existing = Get-StationPid
if ($existing) {
    Write-Host "[station] already running (pid $existing) -> http://127.0.0.1:$Port/   use -Restart to replace it"
    return
}

# 1. ComfyUI (only if the API is not already answering). The producer relaunches it later if it dies.
if (-not (Test-Comfy)) {
    Write-Host "[station] starting ComfyUI"
    Start-Process -FilePath "powershell.exe" -ArgumentList "-NoProfile","-ExecutionPolicy","Bypass","-File",(Join-Path $Root "scripts\run_comfyui.ps1") `
        -RedirectStandardOutput (Join-Path $Root "output\comfyui.log") -RedirectStandardError (Join-Path $Root "output\comfyui.err.log") -WindowStyle Hidden | Out-Null
    for ($i = 0; $i -lt 90; $i++) { Start-Sleep -Seconds 2; if (Test-Comfy) { break } }
}
Write-Host ("[station] ComfyUI: " + $(if (Test-Comfy) { "up" } else { "still starting (the producer will wait and relaunch it if needed)" }))

# 2. Ollama (writer LLM, CPU-pinned so the iGPU stays free for H3). Optional: falls back to built-in scripts.
try { Invoke-RestMethod -Uri http://127.0.0.1:11434/api/tags -TimeoutSec 3 | Out-Null; Write-Host "[station] Ollama OK ($LlmModel)" }
catch { Write-Host "[station] Ollama not answering yet; the station retries per programme (fallback scripts meanwhile)" }

# 3. Station: web player + HLS broadcaster + production loop (forever)
$stationArgs = @("-u", "-m", "aitv", "station", "--host", "127.0.0.1", "--port", "$Port",
                 "--width", "$Width", "--height", "$Height", "--steps", "$Steps", "--llm-model", $LlmModel)
if ($Formats) { $stationArgs += @("--formats", $Formats) }
$env:PYTHONIOENCODING = "utf-8"
$env:PYTHONUTF8 = "1"
$env:AITV_LLM_NUM_GPU = "0"
$p = Start-Process -FilePath $Py -ArgumentList $stationArgs -RedirectStandardOutput (Join-Path $Root "output\station.log") `
        -RedirectStandardError (Join-Path $Root "output\station.err.log") -WindowStyle Hidden -PassThru
Set-Content -Path $PidFile -Value $p.Id -Encoding ascii
Write-Host "[station] started pid $($p.Id)  ->  http://127.0.0.1:$Port/   HLS: http://127.0.0.1:$Port/hls/live.m3u8"
Write-Host "[station] production is ~60x slower than real time on this GPU (a 6.6 s clip takes ~400 s at ${Width}x${Height});"
Write-Host "[station] the channel airs new programmes as soon as they render and mixes the library in between."
