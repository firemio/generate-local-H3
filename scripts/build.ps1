# Build the station binaries: bin\aitv.exe (the station) and bin\h3gen.exe (the generation CLI).
#   .\scripts\build.ps1            # go build -o bin\ .\cmd\...
#   .\scripts\build.ps1 -Test      # also go vet + go test ./...
# Needs Go 1.26+ on PATH (winget install GoLang.Go). Nothing else: the port has one Go dependency
# (gorilla/websocket), fetched into the module cache on the first build.
param([switch]$Test)
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw "Go not found on PATH: winget install GoLang.Go (1.26+)" }
New-Item -ItemType Directory -Force (Join-Path $Root "bin") | Out-Null
Write-Host "[build] $(go version)"
if ($Test) {
    go vet ./...; if ($LASTEXITCODE) { throw "go vet failed" }
    go test ./...; if ($LASTEXITCODE) { throw "go test failed" }
}
go build -o (Join-Path $Root "bin\") ./cmd/...
if ($LASTEXITCODE) { throw "go build failed" }
Get-ChildItem (Join-Path $Root "bin") -Filter *.exe | ForEach-Object { Write-Host ("[build] {0}  {1:N0} bytes" -f $_.Name, $_.Length) }
