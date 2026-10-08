@echo off
setlocal
title XRW Local Uploader - Close this window to stop
where pwsh.exe >nul 2>&1
if errorlevel 1 (
    powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0Start-Local-Uploader.ps1"
) else (
    pwsh.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0Start-Local-Uploader.ps1"
)
if errorlevel 1 (
    echo.
    echo Uploader stopped with an error. See the message above.
    pause
)
endlocal
