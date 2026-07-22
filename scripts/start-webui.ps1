# Start Open WebUI and point it at local sentinel-go (:5005)
# Usage: .\scripts\start-webui.ps1

$ErrorActionPreference = "Stop"
$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)

$Root = Split-Path -Parent $PSScriptRoot
$DataDir = Join-Path $Root "data\open-webui"
$Port = if ($env:WEBUI_PORT) { $env:WEBUI_PORT } else { "3000" }
$ApiBase = if ($env:OPENAI_API_BASE_URL) { $env:OPENAI_API_BASE_URL } else { "http://127.0.0.1:5005/v1" }
$ApiKey = if ($env:OPENAI_API_KEY) { $env:OPENAI_API_KEY } else { "sk-any" }

function Test-PortListening([int]$P) {
    $c = Get-NetTCPConnection -LocalPort $P -State Listen -ErrorAction SilentlyContinue
    return $null -ne $c
}

Write-Host ""
Write-Host "=== Open WebUI <-> sentinel-go ===" -ForegroundColor Cyan
Write-Host "API Base : $ApiBase"
Write-Host "WebUI    : http://127.0.0.1:$Port"
Write-Host "DataDir  : $DataDir"
Write-Host ""

# Check sentinel-go
try {
    $h = Invoke-RestMethod -Uri "http://127.0.0.1:5005/health" -TimeoutSec 3
    Write-Host "[ok] sentinel-go is up (tokens_valid=$($h.tokens_valid))" -ForegroundColor Green
} catch {
    Write-Host "[!] sentinel-go not reachable at :5005" -ForegroundColor Yellow
    Write-Host "    Start it first:  go run ./cmd/server/" -ForegroundColor Yellow
    Write-Host "    Or double-click: scripts\start-all.bat" -ForegroundColor Yellow
    Write-Host ""
}

if (Test-PortListening ([int]$Port)) {
    Write-Host "[ok] port $Port already in use, opening browser..." -ForegroundColor Green
    Start-Process "http://127.0.0.1:$Port"
    exit 0
}

$cmd = Get-Command open-webui -ErrorAction SilentlyContinue
if (-not $cmd) {
    Write-Host "[err] open-webui not found. Install once:" -ForegroundColor Red
    Write-Host "      pip install open-webui" -ForegroundColor Red
    exit 1
}

New-Item -ItemType Directory -Path $DataDir -Force | Out-Null

$env:OPENAI_API_BASE_URL = $ApiBase
$env:OPENAI_API_KEY = $ApiKey
$env:DATA_DIR = $DataDir
# Keep local admin account; set WEBUI_AUTH=false only for fully open local demos
if (-not $env:WEBUI_AUTH) { $env:WEBUI_AUTH = "true" }

Write-Host "[..] starting Open WebUI (Ctrl+C to stop)..." -ForegroundColor Cyan
Start-Process "http://127.0.0.1:$Port"
& open-webui serve --host 127.0.0.1 --port $Port
