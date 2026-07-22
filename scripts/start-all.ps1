# Start sentinel-go API + Open WebUI together (Windows)
# Usage: .\scripts\start-all.ps1

$ErrorActionPreference = "Stop"
$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)

$Root = Split-Path -Parent $PSScriptRoot
$DataDir = Join-Path $Root "data\open-webui"
$LogDir = Join-Path $Root "logs"
$ServerLog = Join-Path $LogDir "sentinel-server.log"
$WebUILog = Join-Path $LogDir "open-webui.log"
$PortApi = if ($env:PORT) { $env:PORT } else { "5005" }
$PortUi = if ($env:WEBUI_PORT) { $env:WEBUI_PORT } else { "3000" }
$ApiBase = "http://127.0.0.1:$PortApi/v1"
$ApiKey = if ($env:OPENAI_API_KEY) { $env:OPENAI_API_KEY } elseif ($env:AUTHORIZATION) { $env:AUTHORIZATION } else { "sk-any" }

New-Item -ItemType Directory -Path $DataDir, $LogDir -Force | Out-Null

function Test-PortListening([int]$P) {
    $c = Get-NetTCPConnection -LocalPort $P -State Listen -ErrorAction SilentlyContinue
    return $null -ne $c
}

function Wait-HttpOk([string]$Url, [int]$Seconds = 60) {
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri $Url -UseBasicParsing -TimeoutSec 2
            if ($r.StatusCode -ge 200 -and $r.StatusCode -lt 500) { return $true }
        } catch {}
        Start-Sleep -Seconds 1
    }
    return $false
}

Write-Host ""
Write-Host "=== start-all: sentinel-go + Open WebUI ===" -ForegroundColor Cyan
Write-Host "API  : http://127.0.0.1:$PortApi"
Write-Host "UI   : http://127.0.0.1:$PortUi"
Write-Host ""

# 1) sentinel-go
if (Test-PortListening ([int]$PortApi)) {
    Write-Host "[ok] sentinel-go already listening on :$PortApi" -ForegroundColor Green
} else {
    Write-Host "[..] starting sentinel-go ..." -ForegroundColor Cyan
    $serverCmd = @"
`$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new(`$false)
Set-Location '$Root'
if (-not `$env:PORT) { `$env:PORT = '$PortApi' }
if (-not `$env:DEFAULT_MODEL) { `$env:DEFAULT_MODEL = 'gpt-5-5-thinking' }
go run ./cmd/server/ *>> '$ServerLog' 2>&1
"@
    Start-Process -FilePath "powershell.exe" -ArgumentList @(
        "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", $serverCmd
    ) -WindowStyle Minimized

    if (-not (Wait-HttpOk "http://127.0.0.1:$PortApi/health" 90)) {
        Write-Host "[err] sentinel-go failed to start. See $ServerLog" -ForegroundColor Red
        exit 1
    }
    Write-Host "[ok] sentinel-go started" -ForegroundColor Green
}

# 2) Open WebUI
$ow = Get-Command open-webui -ErrorAction SilentlyContinue
if (-not $ow) {
    Write-Host "[err] open-webui not found. Run: pip install open-webui" -ForegroundColor Red
    exit 1
}

if (Test-PortListening ([int]$PortUi)) {
    Write-Host "[ok] Open WebUI already listening on :$PortUi" -ForegroundColor Green
} else {
    Write-Host "[..] starting Open WebUI ..." -ForegroundColor Cyan
    $uiCmd = @"
`$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new(`$false)
`$env:OPENAI_API_BASE_URL = '$ApiBase'
`$env:OPENAI_API_KEY = '$ApiKey'
`$env:DATA_DIR = '$DataDir'
if (-not `$env:WEBUI_AUTH) { `$env:WEBUI_AUTH = 'true' }
& open-webui serve --host 127.0.0.1 --port $PortUi *>> '$WebUILog' 2>&1
"@
    Start-Process -FilePath "powershell.exe" -ArgumentList @(
        "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", $uiCmd
    ) -WindowStyle Minimized

    if (-not (Wait-HttpOk "http://127.0.0.1:$PortUi" 120)) {
        Write-Host "[err] Open WebUI failed to start. See $WebUILog" -ForegroundColor Red
        exit 1
    }
    Write-Host "[ok] Open WebUI started" -ForegroundColor Green
}

Write-Host ""
Write-Host "Ready:" -ForegroundColor Green
Write-Host "  Frontend : http://127.0.0.1:$PortUi"
Write-Host "  Backend  : http://127.0.0.1:$PortApi/v1"
Write-Host "  Health   : http://127.0.0.1:$PortApi/health"
Write-Host ""
Write-Host "First visit Open WebUI: register a local admin account,"
Write-Host "then Admin -> Settings -> Connections -> OpenAI API:"
Write-Host "  Base URL = $ApiBase"
Write-Host "  API Key  = $ApiKey"
Write-Host ""
Write-Host "Stop both: scripts\stop-all.bat"
Write-Host ""

Start-Process "http://127.0.0.1:$PortUi"
