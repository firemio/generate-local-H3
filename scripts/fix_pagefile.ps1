# Configure a fixed page file large enough for MiniMax H3 turbo-LoRA generation.
#   scripts\fix_pagefile.ps1 [-InitialGB 48] [-MaximumGB 96] [-Drive C] [-Revert] [-WhatIfOnly]
#
# WHY THIS IS NEEDED
# ---------------------------------------------------------------------------
# On this box the 96 GB memory carve-out leaves Windows with only ~31.6 GB of RAM, so the
# *commit limit* -- not GPU VRAM -- is what limits H3. Measured on 2026-10-13:
#
#   commit limit          = 31.6 GB RAM + page file size
#   ComfyUI peak commit   = 61.10 GB   (measured via PeakPagedMemorySize64 over a full run)
#   page file peak *used* = only ~10 GB
#
# Note the last line: almost the whole 61 GB peak is backed by physical RAM, and the page file
# is barely touched. What matters is that the *limit* is high enough for the allocation to be
# granted -- Windows must be able to promise 61 GB of commit, whether or not it ever pages.
#
# The page file is system-managed ("Automatically manage paging file size for all drives"),
# so Windows sizes it from recent demand. It was observed at 75.10 GB while a run was in
# flight, but shrank back to 9.25 GB after a fresh boot -- which drops the commit limit to
# 40.9 GB, below the 61 GB peak. The turbo-LoRA merge then fails inside comfy_kitchen's
# quantize_int8_convrot_weight with
#
#   torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 148.00 MiB.
#   GPU 0 has a total capacity of 107.87 GiB of which 44.82 GiB is free.
#
# i.e. "44 GB free" yet a 148 MB allocation fails -- the tell-tale sign that this is a host
# commit-limit problem, not GPU memory. The subsequent OOM-recovery path unloads models and
# hits an access violation (0xC0000005), killing ComfyUI outright; h3gen only ever sees
# "websocket closed: [WinError 10054]", which is why the cause is invisible from the client.
# This is the intermittent crash recorded in docs/pitfalls.md section 9.
#
# A fixed minimum floor keeps the limit from collapsing below what a run needs. Because the
# page file is mostly a *guarantee* rather than real backing store, a moderate floor is
# enough -- this does not require provisioning 60+ GB of actual disk use.
#
# NOTES
#   * Requires Administrator (elevates itself via UAC).
#   * A page-file change needs a reboot to take effect.
#   * Do NOT additionally set PYTORCH_HIP_ALLOC_CONF=expandable_segments:True: docs/pitfalls.md
#     section 1 records that it OOMs at ~14 GB on Windows/HIP.
#   * Default target is C:. A second page file on D: also works but C: already has the
#     existing pagefile.sys, so extending it in place is the smaller change.

[CmdletBinding()]
param(
    [int]$InitialGB = 40,        # floor for the commit limit; ComfyUI peaks at ~61 GB total
    [int]$MaximumGB = 96,
    [string]$Drive = "C",
    [switch]$Revert,        # restore system-managed page file
    [switch]$WhatIfOnly     # print what would change, make no changes
)

$ErrorActionPreference = "Stop"

function Assert-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $p = New-Object Security.Principal.WindowsPrincipal($id)
    if (-not $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw "This script must run elevated (Administrator)."
    }
}

function Show-Current {
    $os = Get-CimInstance Win32_OperatingSystem
    $auto = (Get-CimInstance Win32_ComputerSystem).AutomaticManagedPagefile
    Write-Host "  system-managed page file : $auto"
    $pf = Get-CimInstance Win32_PageFileUsage -ErrorAction SilentlyContinue
    foreach ($p in $pf) {
        Write-Host ("  {0,-22} allocated {1,7:N2} GB, peak used {2,6:N2} GB" -f `
            $p.Name, ($p.AllocatedBaseSize/1024), ($p.PeakUsage/1024))
    }
    Write-Host ("  RAM {0:N1} GB | commit limit {1:N1} GB | free commit {2:N1} GB" -f `
        ($os.TotalVisibleMemorySize/1MB), ($os.TotalVirtualMemorySize/1MB), ($os.FreeVirtualMemory/1MB))
    Write-Host ("  ComfyUI peak commit observed : 61.0 GB")
}

if ($WhatIfOnly) {
    Write-Host "=== CURRENT ==="
    Show-Current
    Write-Host ""
    if ($Revert) {
        Write-Host "WOULD: set AutomaticManagedPagefile = True (system-managed)"
    } else {
        Write-Host "WOULD: set AutomaticManagedPagefile = False"
        Write-Host "WOULD: set ${Drive}: page file initial=$InitialGB GB maximum=$MaximumGB GB"
        Write-Host "WOULD: require a reboot"
    }
    exit 0
}

Assert-Admin

Write-Host "=== CURRENT ==="
Show-Current
Write-Host ""

$cs = Get-CimInstance Win32_ComputerSystem

if ($Revert) {
    Write-Host "Restoring the system-managed page file..."
    if (-not $cs.AutomaticManagedPagefile) {
        Set-CimInstance -InputObject $cs -Property @{ AutomaticManagedPagefile = $true }
    }
    # Remove any explicit per-drive setting so Windows takes over again.
    Get-CimInstance Win32_PageFileSetting -ErrorAction SilentlyContinue | ForEach-Object {
        Write-Host "  removing explicit setting for $($_.Name)"
        Remove-CimInstance -InputObject $_
    }
    Write-Host ""
    Write-Host "Done. Reboot to take effect."
    exit 0
}

# Validate before touching anything.
$free = (Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='${Drive}:'").FreeSpace / 1GB
if ($MaximumGB -lt $InitialGB) { throw "MaximumGB ($MaximumGB) must be >= InitialGB ($InitialGB)." }
if ($free -lt ($MaximumGB + 20)) {
    throw ("Not enough free space on ${Drive}: : {0:N1} GB free, want at least {1} GB headroom beyond the {2} GB maximum." -f $free, 20, $MaximumGB)
}

# 1. Turn off system management, otherwise the per-drive setting is ignored.
if ($cs.AutomaticManagedPagefile) {
    Write-Host "Disabling the system-managed page file..."
    Set-CimInstance -InputObject $cs -Property @{ AutomaticManagedPagefile = $false }
} else {
    Write-Host "System-managed page file already disabled."
}

# 2. Create or update the explicit setting for the target drive.
$name = "${Drive}:\pagefile.sys"
$existing = Get-CimInstance Win32_PageFileSetting -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -ieq $name }
if ($existing) {
    Write-Host "Updating existing setting for $name ..."
    Set-CimInstance -InputObject $existing -Property @{ InitialSize = $InitialGB; MaximumSize = $MaximumGB }
} else {
    Write-Host "Creating page file setting for $name ..."
    New-CimInstance -ClassName Win32_PageFileSetting -Property @{
        Name        = $name
        InitialSize = $InitialGB
        MaximumSize = $MaximumGB
    } | Out-Null
}

Write-Host ""
Write-Host "=== CONFIGURED ==="
Get-CimInstance Win32_PageFileSetting -ErrorAction SilentlyContinue |
    Select-Object Name, InitialSize, MaximumSize | Format-Table -AutoSize
Write-Host ""
Write-Host "A reboot is required for this to take effect."
Write-Host "After rebooting, verify with:  Get-CimInstance Win32_OperatingSystem | Select-Object TotalVirtualMemorySize, FreeVirtualMemory"
Write-Host "Expected commit limit: roughly $([math]::Round(31.6 + $InitialGB, 0)) GB or more."
Write-Host ""
Write-Host "Then test turbo mode:"
Write-Host "  cd D:\Apps\generate-local-H3"
Write-Host "  .\.venv\Scripts\python.exe -m h3gen gen --prompt `"a red fox running through snow, cinematic`" --seconds 4 --steps 8"