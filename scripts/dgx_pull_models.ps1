# Pull the H3 model set from the DGX with resume support.
#   scripts\dgx_pull_models.ps1            # everything
#   scripts\dgx_pull_models.ps1 -Only vae,loras
#
# Why HTTP+curl and not scp:
#   * This machine's WiFi link (RSSI -69 dBm, 802.11ac ch44) resets connections
#     every few minutes. scp cannot resume, so one reset discards an entire file
#     (a 27 GB loss). curl -C - continues from the current byte offset.
#   * Measured 3.94 MB/s over HTTP vs 3.10 MB/s over scp.
#   * Single stream on purpose: two parallel streams measured 0.7 MB/s total,
#     because WiFi is a shared medium where extra streams contend for airtime.
#
# The DGX side runs .tools\range_server.py (206 Partial Content) on port 8778.
# Re-running is always safe: complete files are skipped by size.
param(
    [string[]]$Only = @(),
    [string]$Url = "http://192.168.0.86:8778",
    [string]$Local = "models",
    [string]$Key = "$env:LOCALAPPDATA\NVIDIA Corporation\Sync\config\nvsync.key",
    [string]$Target = "fuqiang@192.168.0.86",
    [int]$MaxTries = 40
)
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
$Log = Join-Path $Root "output\pull-models.log"

$raw = (& ssh -i $Key -o StrictHostKeyChecking=no $Target "cd /srv/dspark/h3-models && find . -name '*.safetensors' -printf '%s %p\n' | sort -k2") -join "`n"
$files = @()
foreach ($line in ($raw -split "`r?`n")) {
    if ($line -match '^(\d+)\s+(.+)$') {
        $rel = $Matches[2] -replace '^\./', ''
        if ($Only.Count -and -not ($Only | Where-Object { $rel -like "*$_*" })) { continue }
        $files += [pscustomobject]@{ Rel = $rel; Size = [int64]$Matches[1] }
    }
}
# Smallest first: gets a usable model set on disk sooner.
$files = $files | Sort-Object Size

function Write-Log($msg) {
    $line = "[{0:HH:mm:ss}] {1}" -f (Get-Date), $msg
    Write-Host $line
    Add-Content -Path $Log -Value $line
}

# The AP roams this adapter onto 5 GHz ch149, where RSSI is worse (-71 vs -61) and the
# negotiated receive rate collapses (observed 6.5-39 Mbps vs 300 on 2.4 GHz ch11), taking
# throughput from ~3.8 MB/s down to ~0.7. A reconnect brings it back to the good BSS.
# Preferring the band in the driver needs Administrator, which is not available here.
function Repair-WifiBand {
    param([string]$Ssid = "tulip")
    $iface = (netsh wlan show interfaces) -join "`n"
    $rx = if ($iface -match 'Receive rate \(Mbps\)\s+:\s+([\d.]+)') { [double]$Matches[1] } else { 0 }
    if ($rx -gt 0 -and $rx -lt 100) {
        Write-Log ("      wifi rx {0} Mbps - weak band, reconnecting to {1}" -f $rx, $Ssid)
        & netsh wlan connect name=$Ssid ssid=$Ssid | Out-Null
        Start-Sleep -Seconds 12
        $iface2 = (netsh wlan show interfaces) -join "`n"
        if ($iface2 -match 'Receive rate \(Mbps\)\s+:\s+([\d.]+)') {
            Write-Log ("      wifi now rx {0} Mbps" -f $Matches[1])
        }
    }
}

$total = ($files | Measure-Object Size -Sum).Sum
$have = 0
foreach ($f in $files) {
    $p = Join-Path $Local ($f.Rel -replace '/', '\')
    if ((Test-Path $p) -and (Get-Item $p).Length -eq $f.Size) { $have += $f.Size }
}
Write-Log ("=== {0} files, {1:N2} GB total, {2:N2} GB already local ===" -f $files.Count, ($total/1GB), ($have/1GB))

$failed = @()

foreach ($f in $files) {
    $dest = Join-Path $Local ($f.Rel -replace '/', '\')
    New-Item -ItemType Directory -Force (Split-Path $dest -Parent) | Out-Null
    if ((Test-Path $dest) -and (Get-Item $dest).Length -eq $f.Size) {
        Write-Log "skip  $($f.Rel)"
        continue
    }
    # NOT $url: PowerShell variable names are case-insensitive, so $url and the $Url
    # parameter are the SAME variable. Assigning "$Url/..." to $url therefore rewrote the
    # base for the next iteration, producing paths like
    # "/loras/a.safetensors/loras/b.safetensors/vae/c.safetensors" (all 404).
    $fileUrl = "$Url/$($f.Rel)"
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $try = 0
    $lastSize = [int64]-1
    $stallCount = 0
    while ($true) {
        $try++
        $cur = if (Test-Path $dest) { [int64](Get-Item $dest).Length } else { [int64]0 }
        if ($cur -eq $f.Size) { break }
        # Only count an attempt as stalled once it has run for a while: a curl that dies
        # in milliseconds (link blip, AP roam) must not burn the retry budget instantly.
        # Without this, 9 instant failures used to exhaust MaxTries within one second.
        if ($sw.Elapsed.TotalSeconds -lt 20) { $stallCount = 0 }
        elseif ($cur -le $lastSize) { $stallCount++ }
        else { $stallCount = 0 }
        $lastSize = $cur
        if ($stallCount -ge 2) { Repair-WifiBand }
        if ($stallCount -ge 8) {
            Write-Log "SKIP  $($f.Rel): no progress across 8 attempts at $([math]::Round($cur/1MB,1)) MB"
            $failed += $f.Rel
            break
        }
        if ($try -gt $MaxTries) {
            Write-Log "SKIP  $($f.Rel) after $MaxTries tries"
            $failed += $f.Rel
            break
        }
        if ($cur -eq 0) {
            Write-Log ("pull  {0} ({1:N2} GB)" -f $f.Rel, ($f.Size/1GB))
        } elseif ($try -gt 1) {
            Write-Log ("resume {0} at {1:N2}/{2:N2} GB (try {3})" -f $f.Rel, ($cur/1GB), ($f.Size/1GB), $try)
        }
        # --fail is essential: without it curl writes the *error body* (e.g. a 404 HTML page)
# into the output file and exits 0. That produced 4 files whose size looked right but
# whose contents began with "<!DOCTYP"; a later -C - then appended real data after the
# HTML, so only a header check could detect it.
        # Prefer -C - (resume from the current file size). If the server does not confirm
        # byte ranges for this attempt, curl exits 33 without transferring anything; in that
        # case fall back to an explicit -r range and append, so a transient server hiccup
        # cannot wedge the whole file.
        $viaRange = ($stallCount -ge 2 -and $cur -gt 0)
        if ($viaRange) {
            Write-Log "      (using explicit -r $cur- after repeated resume failures)"
            & curl.exe -sS --fail -o "$dest.part" -r "$cur-" "$fileUrl" `
                --retry 3 --retry-delay 5 --retry-all-errors `
                --speed-limit 30720 --speed-time 120 --connect-timeout 20 2>&1 |
                ForEach-Object { Write-Log "      curl: $_" }
            if ((Test-Path "$dest.part") -and (Get-Item "$dest.part").Length -gt 0) {
                $fs = [System.IO.File]::Open($dest, [System.IO.FileMode]::Append)
                $ps = [System.IO.File]::OpenRead("$dest.part")
                $ps.CopyTo($fs); $ps.Close(); $fs.Close()
            }
            Remove-Item "$dest.part" -Force -ErrorAction SilentlyContinue
        } else {
            # No --max-time: a large file legitimately takes hours. --speed-limit aborts a
            # genuinely dead connection so the outer loop can resume it.
            & curl.exe -sS --fail -C - -o "$dest" "$fileUrl" `
                --retry 3 --retry-delay 5 --retry-all-errors `
                --speed-limit 30720 --speed-time 120 `
                --connect-timeout 20 2>&1 | ForEach-Object { Write-Log "      curl: $_" }
        }
        # A non-safetensors start means an error body was written: drop it and start over.
        if ((Test-Path $dest) -and (Get-Item $dest).Length -gt 0) {
            $fh = [System.IO.File]::OpenRead($dest)
            $magic = New-Object byte[] 8
            if ($fh.Read($magic, 0, 8) -eq 8) {
                $flen = [System.BitConverter]::ToInt64($magic, 0)
                if ($flen -le 0 -or $flen -gt 200MB) {
                    Write-Log "      (garbage header detected - restarting this file)"
                    $fh.Close()
                    Remove-Item $dest -Force
                } else { $fh.Close() }
            } else { $fh.Close() }
        }
        # Back off between attempts so a flapping link has time to settle.
        if ($try -gt 1) { Start-Sleep -Seconds ([math]::Min(5 * $try, 30)) }
    }
    $sw.Stop()
    if (-not (Test-Path $dest) -or (Get-Item $dest).Length -ne $f.Size) { continue }   # already reported as SKIP
    $rate = $f.Size / 1MB / [math]::Max($sw.Elapsed.TotalSeconds, 0.001)
    Write-Log ("done  {0} {1:N2} GB in {2} min ({3:N2} MB/s, {4} tries)" -f `
        $f.Rel, ($f.Size/1GB), $sw.Elapsed.TotalMinutes.ToString('0.0'), $rate, $try)
}
if ($failed.Count) {
    Write-Log "=== INCOMPLETE: $($failed.Count) file(s) failed ==="
    foreach ($n in $failed) { Write-Log "   $n" }
    exit 1
}
Write-Log "=== ALL FILES COMPLETE ==="