# Stop Open WebUI and sentinel-go server processes started for this project
# Usage: .\scripts\stop-all.ps1

$ErrorActionPreference = "Continue"
$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)

Write-Host "=== stop-all ===" -ForegroundColor Cyan

# Open WebUI (python / open-webui)
Get-CimInstance Win32_Process -Filter "Name='open-webui.exe'" -ErrorAction SilentlyContinue |
    ForEach-Object {
        Write-Host "kill open-webui pid=$($_.ProcessId)"
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
    }

# uvicorn / python serving open-webui
Get-CimInstance Win32_Process -Filter "Name='python.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match "open.webui|open_webui|open-webui" } |
    ForEach-Object {
        Write-Host "kill python(open-webui) pid=$($_.ProcessId)"
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
    }

# sentinel-go: go run / compiled server on 5005
$listeners = Get-NetTCPConnection -LocalPort 5005,3000 -State Listen -ErrorAction SilentlyContinue
foreach ($l in $listeners) {
    $procId = $l.OwningProcess
    if ($procId -and $procId -ne 0) {
        $p = Get-CimInstance Win32_Process -Filter "ProcessId=$procId" -ErrorAction SilentlyContinue
        if ($p) {
            Write-Host "kill port $($l.LocalPort) pid=$procId name=$($p.Name)"
            Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue
        }
    }
}

# go run helper windows that may remain
Get-CimInstance Win32_Process -Filter "Name='go.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match "cmd/server|sentinel-server" } |
    ForEach-Object {
        Write-Host "kill go pid=$($_.ProcessId)"
        Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
    }

Start-Sleep -Seconds 1
$left5005 = Get-NetTCPConnection -LocalPort 5005 -State Listen -ErrorAction SilentlyContinue
$left3000 = Get-NetTCPConnection -LocalPort 3000 -State Listen -ErrorAction SilentlyContinue
if ($left5005 -or $left3000) {
    Write-Host "[!] some ports still listening" -ForegroundColor Yellow
} else {
    Write-Host "[ok] ports 5005/3000 free" -ForegroundColor Green
}
