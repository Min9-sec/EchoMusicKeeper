$ErrorActionPreference = "Stop"
$goVersion = go version
if ($goVersion -notmatch "\bgo1\.25\.4\b") {
  throw "Go 1.25.4 is required; found: $goVersion"
}
$packagePath = Join-Path $PSScriptRoot "..\package.json"
$package = Get-Content -Raw $packagePath | ConvertFrom-Json
$version = [string]$package.version
if ($version -notmatch '^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$') {
  throw "package.json contains an invalid version: $version"
}
$ldflags = "-s -w -X main.version=$version"
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
$output = Join-Path $PSScriptRoot "..\bin\echo-music-keeper-helper.exe"
Push-Location $PSScriptRoot
try {
  go build -buildvcs=false -trimpath -ldflags $ldflags -o $output ./cmd/echo-music-keeper-helper
} finally {
  Pop-Location
}
if (-not (Test-Path $output)) { throw "echo-music-keeper-helper.exe was not created" }
Get-FileHash -Algorithm SHA256 $output
