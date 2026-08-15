@echo off
setlocal
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0run-muse-glimmer-64k.ps1" %*
exit /b %ERRORLEVEL%
