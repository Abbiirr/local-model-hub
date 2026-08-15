@echo off
REM Convenience wrapper for the documented PowerShell launcher.
REM Examples:
REM   scripts\run-qwen-64k-q4q4.cmd
REM   scripts\run-qwen-64k-q4q4.cmd Status
REM   scripts\run-qwen-64k-q4q4.cmd Restart
REM   scripts\run-qwen-64k-q4q4.cmd Stop

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0run-qwen-64k-q4q4.ps1" %*
