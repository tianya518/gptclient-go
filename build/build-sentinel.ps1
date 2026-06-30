# 编译 sentinel-server.exe
# 用法: powershell -ExecutionPolicy Bypass -File build\build-sentinel.ps1 [-OutDir path] [-Upx]

param(
    [string]$OutDir = "",
    [switch]$Upx
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

$Root = Split-Path -Parent $PSScriptRoot
if ($OutDir -eq "") {
    $OutDir = Join-Path $Root "bin"
}
if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Path $OutDir | Out-Null }

$Exe = Join-Path $OutDir "sentinel-server.exe"

Write-Host ">> building sentinel-server -> $Exe" -ForegroundColor Cyan
Push-Location $Root
try {
    $env:CGO_ENABLED = "0"
    $env:GOOS = "windows"
    go build -trimpath -ldflags="-w -s" -o $Exe ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }

    if ($Upx -and (Get-Command upx -ErrorAction SilentlyContinue)) {
        Write-Host ">> UPX compress..." -ForegroundColor Cyan
        upx --best --lzma $Exe
    } elseif ($Upx) {
        Write-Host ">> UPX not found, skip (winget install UPX.UPX)" -ForegroundColor Yellow
    }

    Copy-Item -Force $Exe (Join-Path $Root "sentinel-server.exe")
    Write-Host ">> Done" -ForegroundColor Green
} finally {
    Pop-Location
}
