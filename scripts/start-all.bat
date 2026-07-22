@echo off
chcp 65001 >nul
cd /d "%~dp0.."
echo Starting sentinel-go + Open WebUI ...
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start-all.ps1"
echo.
pause
