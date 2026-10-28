# Stream a directory from the DGX Spark to this machine over SSH + tar.
#   scripts\dgx_pull.ps1 -Remote /srv/dspark/h3-wheels -Local .tools\wheels -Name wheels
#   scripts\dgx_pull.ps1 -Remote /srv/dspark/h3-models -Local models -Name models -Exclude "*.part" -Exclude "download.log"
#
# Why not rsync: Windows OpenSSH here ships scp/ssh but no rsync, and WSL is not
# usable from this sandbox. tar-over-ssh needs no local install and no temp file.
# No compression on purpose: these payloads are already-compressed weights.
param(
    [Parameter(Mandatory = $true)][string]$Remote,
    [Parameter(Mandatory = $true)][string]$Local,
    [Parameter(Mandatory = $true)][string]$Name,
    [string[]]$Exclude = @(),
    [string]$Key = "$env:LOCALAPPDATA\NVIDIA Corporation\Sync\config\nvsync.key",
    [string]$Target = "fuqiang@192.168.0.86"
)
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
$LogDir = Join-Path $Root "output"
New-Item -ItemType Directory -Force $LogDir, $Local | Out-Null
$stderrLog = Join-Path $LogDir "pull-$Name.err.log"
# ssh/tar stderr is redirected into a variable's file by the caller; PowerShell cannot append
# two streams of one pipeline to the same file (the handle stays open), so the error text of
# the transport is kept in a separate file written only on failure.
$exArgs = ($Exclude | ForEach-Object { "--exclude=$_" }) -join " "
$remoteCmd = "tar cf - $exArgs -C $Remote ."
$LocalAbs = (Resolve-Path $Local).Path
$sw = [System.Diagnostics.Stopwatch]::StartNew()
$errFile = [System.IO.Path]::GetTempFileName()
& ssh -i $Key -o StrictHostKeyChecking=no $Target $remoteCmd 2> $errFile |
    & tar xf - -C $LocalAbs
$code = $LASTEXITCODE
$sw.Stop()
if ($code -ne 0) {
    Copy-Item $errFile $stderrLog -Force
    Write-Host "!!! tar/ssh exited $code after $($sw.Elapsed.TotalMinutes.ToString('0.0')) min (see $stderrLog)"
    exit $code
}
Remove-Item $errFile -Force -ErrorAction SilentlyContinue
$size = (Get-ChildItem -Recurse -File $LocalAbs | Measure-Object -Property Length -Sum).Sum
[System.IO.File]::WriteAllText((Join-Path $LogDir "pull-$Name.log"),
    "pulled $Remote -> $LocalAbs in $($sw.Elapsed.TotalMinutes.ToString('0.0')) min; $([math]::Round($size/1GB,2)) GB")
Write-Host "=== $Name pulled in $($sw.Elapsed.TotalMinutes.ToString('0.0')) min; $([math]::Round($size/1GB,2)) GB ==="
