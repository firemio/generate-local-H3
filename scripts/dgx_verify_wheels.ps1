# Verify every local wheel against the DGX's copy byte-for-byte (size only, no transfer).
#   scripts\dgx_verify_wheels.ps1
# Rationale: the first wheelhouse pull ran while `pip download` on the DGX was still
# writing files, so two wheels arrived as 0-byte files and pip happily installed a
# truncated rocm-sdk-core (0.05 MB of a 758 MB wheel). Size equality against the
# source is the cheapest reliable gate before installing.
param(
    [string]$Remote = "/srv/dspark/h3-wheels",
    [string]$Local = ".tools\wheels",
    [string]$Key = "$env:LOCALAPPDATA\NVIDIA Corporation\Sync\config\nvsync.key",
    [string]$Target = "fuqiang@192.168.0.86"
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

# `stat -c '%s %n'` emits one line per file; ssh may deliver it as one string, so split on newlines.
$raw = (& ssh -i $Key -o StrictHostKeyChecking=no $Target "cd $Remote && stat -c '%s %n' *.whl") -join "`n"
$rMap = @{}
foreach ($line in ($raw -split "`r?`n")) {
    if ($line -match '^(\d+)\s+(.+)$') { $rMap[$Matches[2].Trim()] = [int64]$Matches[1] }
}
$localFiles = Get-ChildItem $Local -File -Filter *.whl
$bad = @()
foreach ($f in $localFiles) {
    if (-not $rMap.ContainsKey($f.Name)) { $bad += "$($f.Name): not present on DGX (local=$($f.Length))"; continue }
    if ($rMap[$f.Name] -ne $f.Length) { $bad += "$($f.Name): local=$($f.Length) remote=$($rMap[$f.Name])" }
}
$missing = $rMap.Keys | Where-Object { $localFiles.Name -notcontains $_ }

"local=$($localFiles.Count)  remote=$($rMap.Count)  mismatched=$($bad.Count)  missing=$($missing.Count)"
foreach ($b in $bad) { "  MISMATCH $b" }
foreach ($m in $missing) { "  MISSING  $m" }
if (-not $bad -and -not $missing) { "ALL WHEELS VERIFIED" }