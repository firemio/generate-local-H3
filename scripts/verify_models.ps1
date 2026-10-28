# Validate that downloaded .safetensors files are structurally sound.
#   scripts\verify_models.ps1
#
# A safetensors file starts with an 8-byte little-endian header length, followed by
# that many bytes of JSON. Anything else (e.g. an HTML error page beginning "<!DOCTYP")
# means the file was written from a failed response and must be re-downloaded.
# This caught a real corruption: a 330-byte HTML error page that curl then appended
# to via -C -, producing a file whose size looked plausible but whose contents did not.
param([string]$Local = "models")
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

$bad = @()
$dirs = @("diffusion_models", "text_encoders", "vae", "loras")
foreach ($d in $dirs) {
    $dir = Join-Path $Local $d
    if (-not (Test-Path $dir)) { continue }
    foreach ($f in (Get-ChildItem $dir -Filter *.safetensors -File)) {
        # A file being written by an in-flight curl is locked; skip it rather than fail,
        # since it is by definition not finished yet.
        $fs = $null
        try { $fs = [System.IO.File]::OpenRead($f.FullName) }
        catch [System.IO.IOException] {
            "{0,-10} {1,10:N1} MB  {2}" -f "IN_USE", ($f.Length/1MB), $f.FullName.Replace("$PWD\", "")
            continue
        }
        $hdr = New-Object byte[] 8
        $read = $fs.Read($hdr, 0, 8)
        $status = "?"
        if ($read -lt 8) {
            $status = "TOO_SMALL"
        } else {
            $len = [System.BitConverter]::ToInt64($hdr, 0)
            # Sanity: header JSON is a few KB at most, and must start with '{'.
            if ($len -le 0 -or $len -gt 200MB -or ($len + 8) -gt $f.Length) {
                $status = "BAD_HEADER_LEN($len)"
            } else {
                $json = New-Object byte[] $len
                $fs.Position = 8
                $null = $fs.Read($json, 0, [int]$len)
                $txt = [System.Text.Encoding]::UTF8.GetString($json)
                if ($txt.TrimStart()[0] -ne '{') { $status = "NOT_JSON" } else { $status = "OK" }
            }
        }
        $fs.Close()
        "{0,-10} {1,10:N1} MB  {2}" -f $status, ($f.Length/1MB), $f.FullName.Replace("$PWD\", "")
        if ($status -ne "OK") { $bad += $f.FullName }
    }
}
Write-Host ""
if ($bad.Count) {
    Write-Host "$($bad.Count) file(s) INVALID - delete and re-download:"
    foreach ($b in $bad) { Write-Host "   $b" }
    exit 1
}
Write-Host "All safetensors files are structurally valid"