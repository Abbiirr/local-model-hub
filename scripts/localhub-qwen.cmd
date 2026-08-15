@echo off
REM Durable LocalHub + Qwen3.6 availability controls.
REM First setup:  scripts\localhub-qwen.cmd Install
REM Normal use:   scripts\localhub-qwen.cmd Status
REM               scripts\localhub-qwen.cmd Up
REM               scripts\localhub-qwen.cmd Down
REM               scripts\localhub-qwen.cmd DownAll

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0keep-localhub-qwen.ps1" %*
