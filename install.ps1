# Frostgate / Frostcode installer (Windows, PowerShell)
# Builds both binaries, installs them to %LOCALAPPDATA%\Programs\frostgate,
# copies config.json there, adds that dir to the user PATH, and sets
# FROSTCODE_CONFIG so `frostcode` works from any project directory.
#
# Usage (from the repo root):
#   powershell -ExecutionPolicy Bypass -File .\install.ps1

$ErrorActionPreference = "Stop"

$repo    = $PSScriptRoot
$dest    = Join-Path $env:LOCALAPPDATA "Programs\frostgate"
$cfgDest = Join-Path $dest "config.json"

Write-Host "Building binaries..." -ForegroundColor Cyan
Push-Location $repo
go build -o (Join-Path $dest "frostgate.exe") ./cmd/frostgate
go build -o (Join-Path $dest "frostcode.exe") ./cmd/frostcode
Pop-Location

# Copy config if present and not already installed.
if ((Test-Path (Join-Path $repo "config.json")) -and -not (Test-Path $cfgDest)) {
    Copy-Item (Join-Path $repo "config.json") $cfgDest
    Write-Host "Copied config.json -> $cfgDest" -ForegroundColor Green
} elseif (Test-Path $cfgDest) {
    Write-Host "Keeping existing $cfgDest" -ForegroundColor Yellow
}

# Add install dir to the USER Path (safe append via .NET, no 1024 truncation).
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dest*") {
    $newPath = if ([string]::IsNullOrEmpty($userPath)) { $dest } else { "$userPath;$dest" }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Host "Added $dest to your user PATH" -ForegroundColor Green
} else {
    Write-Host "PATH already contains $dest" -ForegroundColor Yellow
}

# Point FROSTCODE_CONFIG at the installed config.
[Environment]::SetEnvironmentVariable("FROSTCODE_CONFIG", $cfgDest, "User")
Write-Host "Set FROSTCODE_CONFIG = $cfgDest" -ForegroundColor Green

Write-Host ""
Write-Host "Done. Open a NEW terminal, then run:" -ForegroundColor Cyan
Write-Host "  frostcode            # coding agent in the current folder"
Write-Host "  frostgate            # start the gateway + dashboard"
