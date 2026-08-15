[CmdletBinding()]
param(
    [string]$OutputPath = ''
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $OutputPath) {
    $OutputPath = Join-Path $repoRoot 'localhub-tray.exe'
}

$go = Get-Command go -ErrorAction Stop

# No C toolchain is required. fyne.io/systray's Windows backend is pure Go (it calls user32/
# shell32 through golang.org/x/sys/windows), unlike its Linux/macOS backends. This script used to
# demand gcc and fail outright without it, which blocked the build on any machine that had Go but
# no MSYS2. CGO is pinned off so the result is reproducible regardless of what is on PATH.
#
# -H=windowsgui is mandatory, not cosmetic: without it the tray is a console-subsystem binary and
# Windows opens a console window behind the tray icon.
$previousCGO = $env:CGO_ENABLED
try {
    $env:CGO_ENABLED = '0'
    & $go.Source build `
        -trimpath `
        -ldflags '-H=windowsgui -s -w' `
        -o $OutputPath `
        ./cmd/localhub-tray
    if ($LASTEXITCODE -ne 0) {
        throw "Tray build failed with exit code $LASTEXITCODE."
    }
}
finally {
    $env:CGO_ENABLED = $previousCGO
}

Get-Item -LiteralPath $OutputPath | Select-Object FullName, Length, LastWriteTime
