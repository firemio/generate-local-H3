# Report H3 model transfer progress and readiness.
#   scripts\h3_status.ps1
# Read-only: inspects local files against the DGX manifest and reports what is
# missing, so the end-to-end run can start as soon as the set is complete.
param(
    [string]$Url = "http://192.168.0.86:8778",
    [string]$Local = "models",
    [string]$Key = "$env:LOCALAPPDATA\NVIDIA Corporation\Sync\config\nvsync.key",
    [string]$Target = "fuqiang@192.168.0.86"
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

$raw = (& ssh -i $Key -o StrictHostKeyChecking=no $Target "cd /srv/dspark/h3-models && find . -name '*.safetensors' -printf '%s %p\n' | sort -k2") -join "`n"
$files = @()
foreach ($line in ($raw -split "`r?`n")) {
    if ($line -match '^(\d+)\s+(.+)$') {
        $files += [pscustomobject]@{ Rel = ($Matches[2] -replace '^\./', ''); Size = [int64]$Matches[1] }
    }
}

$totalSize = [int64]0; $doneSize = [int64]0; $pending = @()
foreach ($f in $files) {
    $p = Join-Path $Local ($f.Rel -replace '/', '\')
    $have = if (Test-Path $p) { [int64](Get-Item $p).Length } else { [int64]0 }
    $totalSize += $f.Size
    # Min/Max on this box resolve to Int32 overloads, which overflow on >2 GB files.
    $doneSize += $(if ($have -lt $f.Size) { $have } else { $f.Size })
    $state = if ($have -eq $f.Size) { "OK" } elseif ($have -eq 0) { "MISSING" } else { "PARTIAL" }
    "{0,-9} {1,10:N2} / {2,10:N2} GB  {3}" -f $state, ($have/1GB), ($f.Size/1GB), $f.Rel
    if ($state -ne "OK") { $pending += $f }
}

Write-Host ""
Write-Host ("TOTAL  {0:N2} / {1:N2} GB  ({2:N1}% complete)" -f ($doneSize/1GB), ($totalSize/1GB), (100*$doneSize/$totalSize))
if ($pending.Count -eq 0) {
    Write-Host "ALL MODELS PRESENT - ready for end-to-end generation"
} else {
    Write-Host "$($pending.Count) file(s) pending"
}

# Live transfer state, if a pull is running.
$curl = Get-CimInstance Win32_Process -Filter "Name='curl.exe'" -ErrorAction SilentlyContinue
if ($curl) {
    $names = $curl | ForEach-Object { ($_.CommandLine -split '-o ')[1].Split(' ')[0] }
    Write-Host "transfer active: $($names -join ', ')"
} else {
    Write-Host "no active transfer"
}

# Is ComfyUI up and does it see the GPU?
try {
    $s = Invoke-RestMethod -Uri "http://127.0.0.1:8188/system_stats" -TimeoutSec 8
    Write-Host "ComfyUI: up - $($s.devices[0].name), VRAM $([math]::Round($s.devices[0].vram_total/1GB,1)) GB"
} catch {
    Write-Host "ComfyUI: down"
}
try {
    # Probe a real file: the server 404s on the bare root, which is not a failure.
    $probe = ($files | Select-Object -First 1).Rel
    $r = Invoke-WebRequest -Uri "$Url/$probe" -Headers @{ Range = "bytes=0-99" } -TimeoutSec 8 -UseBasicParsing
    Write-Host "model server: up (HTTP $($r.StatusCode) - $($r.Headers['Content-Range']))"
} catch {
    Write-Host "model server: DOWN ($($_.Exception.Message))"
}

# The AP roams between 2.4 GHz ch11 (~77% signal, 300 Mbps) and 5 GHz ch149 (~57%,
# and observed collapsing to a 6.5 Mbps receive rate). Throughput falls from ~2.5 MB/s
# to ~0.4 MB/s on the bad band, so flag it: 'netsh wlan connect name=tulip ssid=tulip'
# brings it back. Preferring the band in the driver needs Administrator.
$iface = (netsh wlan show interfaces) -join "`n"
if ($iface -match 'Band\s+:\s+(\S+ ?\S*)') {
    $band = $Matches[1].Trim()
    $rateRx = if ($iface -match 'Receive rate \(Mbps\)\s+:\s+(\S+)') { [int][double]$Matches[1] } else { 0 }
    $rssi = if ($iface -match 'Rssi\s+:\s+(-?\d+)') { [int]$Matches[1] } else { 0 }
    Write-Host "wifi: $band, rx $rateRx Mbps, rssi $rssi dBm"
    if ($rateRx -gt 0 -and $rateRx -lt 100) {
        Write-Host "  WARNING: weak receive rate - run: netsh wlan connect name=tulip ssid=tulip"
    }
}