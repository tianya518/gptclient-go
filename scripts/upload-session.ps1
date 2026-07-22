# 把 chatgpt.com/api/auth/session 整页 JSON 保存为 session.json 后运行本脚本
# 或直接把 JSON 粘贴到下面的 $raw 字符串里再运行

param(
  [string]$SessionFile = ""
)

$ErrorActionPreference = "Stop"
$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)

if ($SessionFile -and (Test-Path $SessionFile)) {
  $raw = [System.IO.File]::ReadAllText((Resolve-Path $SessionFile), [System.Text.Encoding]::UTF8)
} else {
  Write-Host "Usage: .\scripts\upload-session.ps1 -SessionFile path\to\session.json"
  Write-Host "Or save session JSON as .\session.json and run without args."
  if (Test-Path ".\session.json") {
    $raw = [System.IO.File]::ReadAllText((Resolve-Path ".\session.json"), [System.Text.Encoding]::UTF8)
  } else {
    exit 1
  }
}

$bodyObj = @{ tokens = $raw }
$body = $bodyObj | ConvertTo-Json -Compress -Depth 5
$resp = Invoke-RestMethod -Uri "http://127.0.0.1:5005/tokens/upload" -Method Post -ContentType "application/json; charset=utf-8" -Body ([System.Text.Encoding]::UTF8.GetBytes($body))
$resp | ConvertTo-Json -Compress
$status = Invoke-RestMethod -Uri "http://127.0.0.1:5005/tokens" -Method Get
Write-Host "pool: total=$($status.total) valid=$($status.valid)"