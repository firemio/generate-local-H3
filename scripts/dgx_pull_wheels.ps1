# Pull selected files from the DGX wheelhouse using tar-over-ssh.
#   scripts\dgx_pull_wheels.ps1
# tar -T takes a list file, which avoids scp's argument-quoting problems with the
# space in the NVIDIA Sync key path and with many remote paths.
param(
    [string]$Remote = "/srv/dspark/h3-wheels",
    [string]$Local = ".tools\wheels",
    [string]$Key = "$env:LOCALAPPDATA\NVIDIA Corporation\Sync\config\nvsync.key",
    [string]$Target = "fuqiang@192.168.0.86"
)
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
New-Item -ItemType Directory -Force $Local | Out-Null
$stamp = Get-Date -Format "yyyyMMddHHmmss"
$listRemote = "/tmp/whl-list-$stamp.txt"
$listLocal = Join-Path $env:TEMP "whl-list-$stamp.txt"

# Build the remote file list, then push it to the DGX.
$remoteNames = & ssh -i $Key -o StrictHostKeyChecking=no $Target "ls $Remote/*.whl | xargs -n1 basename"
$localNames = (Get-ChildItem $Local -File -ErrorAction SilentlyContinue).Name
$missing = $remoteNames | Where-Object { $_ -and ($localNames -notcontains $_) }
$missing = $missing | Where-Object { $_ -notmatch '^torchvision-' }   # keep the ROCm 0.26.0 we installed
if (-not $missing) { Write-Host "nothing missing"; exit 0 }
Write-Host "pulling $($missing.Count) wheels"
# The remote tar -T reads names verbatim: CRLF would make every entry "\r"-suffixed and fail.
[System.IO.File]::WriteAllText($listLocal, (($missing -join "`n") + "`n"))
& scp -i $Key -o StrictHostKeyChecking=no $listLocal "${Target}:$listRemote"
& ssh -i $Key -o StrictHostKeyChecking=no $Target "tar cf - -C $Remote -T $listRemote" |
    & tar xf - -C (Resolve-Path $Local).Path
if ($LASTEXITCODE -ne 0) { Write-Host "transfer failed: $LASTEXITCODE"; exit $LASTEXITCODE }
& ssh -i $Key -o StrictHostKeyChecking=no $Target "rm -f $listRemote"
"{0:N2} GB / {1} files" -f (((Get-ChildItem $Local -File) | Measure-Object Length -Sum).Sum / 1GB), (Get-ChildItem $Local -File).Count