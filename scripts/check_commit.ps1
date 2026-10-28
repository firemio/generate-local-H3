# Check whether the commit limit is large enough for H3 turbo-LoRA generation.
#   scripts\check_commit.ps1
# Read-only. Run this before and after fixing the page file (scripts\fix_pagefile.ps1).
#
# ComfyUI's peak commit during an H3 run was measured at 61.0 GB. On this box only ~31.6 GB
# of the RAM is visible to Windows (the rest is the GPU carve-out), so the rest has to come
# from the page file. If the limit is short, the turbo-LoRA merge fails with a misleading
# "CUDA out of memory ... 44.82 GiB is free" and then kills ComfyUI via access violation.

$ErrorActionPreference = "Stop"

$os = Get-CimInstance Win32_OperatingSystem
$cs = Get-CimInstance Win32_ComputerSystem

$ramGB    = $os.TotalVisibleMemorySize / 1MB
$limitGB  = $os.TotalVirtualMemorySize / 1MB
$freeGB   = $os.FreeVirtualMemory / 1MB
$pfUsed   = $null
$pfAlloc  = 0
foreach ($p in (Get-CimInstance Win32_PageFileUsage -ErrorAction SilentlyContinue)) {
    $pfAlloc += $p.AllocatedBaseSize / 1024
    Write-Host ("page file {0}: allocated {1:N2} GB, peak used {2:N2} GB" -f `
        $p.Name, ($p.AllocatedBaseSize/1024), ($p.PeakUsage/1024))
}

# What already-running software holds, so we can judge headroom rather than just the limit.
$committed = (Get-Process | Measure-Object PagedMemorySize64 -Sum).Sum / 1GB

$NEED = 61.1   # ComfyUI peak commit over a full run, measured 2026-10-13

Write-Host ""
Write-Host ("RAM visible to Windows : {0:N1} GB" -f $ramGB)
Write-Host ("page file allocated    : {0:N1} GB  (system-managed: {1})" -f $pfAlloc, $cs.AutomaticManagedPagefile)
Write-Host ("commit limit           : {0:N1} GB" -f $limitGB)
Write-Host ("commit currently used  : {0:N1} GB  (all processes)" -f $committed)
Write-Host ("commit headroom now    : {0:N1} GB" -f $freeGB)
Write-Host ("needed by ComfyUI peak : {0:N1} GB" -f $NEED)
Write-Host ""

if ($limitGB -ge ($committed + $NEED)) {
    Write-Host "VERDICT: OK - the limit has room for a ComfyUI run alongside current usage."
    exit 0
}

$short = ($committed + $NEED) - $limitGB
Write-Host ("VERDICT: TOO SMALL - short by about {0:N1} GB at current usage." -f $short)
Write-Host ""
Write-Host "Fix (needs Administrator + a reboot):"
Write-Host "  .\scripts\fix_pagefile.ps1 -InitialGB $([math]::Ceiling(($short + $pfAlloc)/8)*8) -MaximumGB 96"
Write-Host ""
Write-Host "Until then, skip the turbo LoRA:"
Write-Host "  .\bin\h3gen.exe gen --prompt `"...`" --seconds 4 --steps 20 --no-turbo"
exit 1