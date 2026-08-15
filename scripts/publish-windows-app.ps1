<#
.SYNOPSIS
  Build, publish, and relaunch the current LocalHub Windows app distribution.

.DESCRIPTION
  Builds the daemon and native tray to side-by-side temporary files first. Only after both builds
  succeed does it stop the existing tray, ask the new native tray binary to drain models and stop
  the verified daemon, promote both binaries at their stable paths, refresh the Start Menu link,
  and relaunch. The stable Start Menu target therefore always resolves to the last complete build.
#>
[CmdletBinding()]
param(
    [int]$ReadyTimeoutSeconds = 360
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path -LiteralPath (Split-Path -Parent $PSScriptRoot)).Path
$trayTarget = Join-Path $repoRoot 'localhub-tray.exe'
$daemonTarget = Join-Path $repoRoot 'bin\localhub.exe'
$daemonFallback = Join-Path $repoRoot 'localhub.exe'
$trayNext = Join-Path $repoRoot ('.publish-localhub-tray-{0}.exe' -f $PID)
$daemonNext = Join-Path $repoRoot ('bin\.publish-localhub-{0}.exe' -f $PID)

function Get-ExactProcess {
    param([Parameter(Mandatory)][string]$ExecutablePath)
    Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
        Where-Object { [string]::Equals($_.ExecutablePath, $ExecutablePath, [StringComparison]::OrdinalIgnoreCase) }
}

function Test-LocalHubHealth {
    try {
        $health = Invoke-RestMethod -Uri 'http://127.0.0.1:39876/api/health' -TimeoutSec 3
        return $health.status -eq 'ok' -and [int]$health.pid -gt 0
    }
    catch {
        return $false
    }
}

try {
    # Fail before touching the live stack when the volume cannot hold both complete artifacts.
    $drive = Get-PSDrive -Name ([IO.Path]::GetPathRoot($repoRoot).Substring(0, 1))
    if ($drive.Free -lt 80MB) {
        throw "Not enough free space to stage a complete Windows build: $([math]::Round($drive.Free / 1MB)) MiB free; at least 80 MiB required."
    }

    & go build -trimpath -o $daemonNext .\cmd\localhub
    if ($LASTEXITCODE -ne 0) { throw "LocalHub daemon build failed with exit code $LASTEXITCODE." }
    & (Join-Path $PSScriptRoot 'build-localhub-tray.ps1') -OutputPath $trayNext

    # Stop only this checkout's tray so its recovery loop cannot race the shutdown helper.
    Get-ExactProcess -ExecutablePath $trayTarget | ForEach-Object {
        Stop-Process -Id $_.ProcessId -Force
        Wait-Process -Id $_.ProcessId -Timeout 15 -ErrorAction SilentlyContinue
    }

    if (Test-LocalHubHealth) {
        $shutdown = Start-Process -FilePath $trayNext `
            -ArgumentList @('-shutdown', '-repo', "`"$repoRoot`"") `
            -WorkingDirectory $repoRoot -WindowStyle Hidden -PassThru -Wait
        if ($shutdown.ExitCode -ne 0) {
            throw "Native LocalHub shutdown failed with exit code $($shutdown.ExitCode); existing binaries were not replaced. See logs\tray.log."
        }
    }

    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $daemonTarget) | Out-Null
    Copy-Item -LiteralPath $daemonNext -Destination $daemonTarget -Force
    Copy-Item -LiteralPath $daemonNext -Destination $daemonFallback -Force
    Copy-Item -LiteralPath $trayNext -Destination $trayTarget -Force
    & (Join-Path $PSScriptRoot 'install-start-menu.ps1') -RepoRoot $repoRoot

    Start-Process -FilePath $trayTarget -ArgumentList @('-repo', "`"$repoRoot`"") `
        -WorkingDirectory $repoRoot -WindowStyle Hidden

    $startup = Get-Content -LiteralPath (Join-Path $repoRoot 'configs\startup.json') -Raw | ConvertFrom-Json
    $enabledModels = @($startup.models | Where-Object enabled | ForEach-Object model_id)
    $deadline = (Get-Date).AddSeconds($ReadyTimeoutSeconds)
    do {
        if (Test-LocalHubHealth) {
            if ($enabledModels.Count -eq 0) { break }
            try {
                $instances = (Invoke-WebRequest -UseBasicParsing `
                    -Uri 'http://127.0.0.1:39876/api/instances' -TimeoutSec 5).Content | ConvertFrom-Json
                $healthy = @($instances | Where-Object { $_.state -eq 'HEALTHY' } | ForEach-Object model_id)
                $missing = @($enabledModels | Where-Object { $_ -notin $healthy })
                if ($missing.Count -eq 0) { break }
            }
            catch { }
        }
        Start-Sleep -Seconds 2
    } while ((Get-Date) -lt $deadline)

    if ((Get-Date) -ge $deadline) {
        throw "Published LocalHub, but configured startup models did not become healthy within $ReadyTimeoutSeconds seconds. See logs\tray.log."
    }

    Get-Item -LiteralPath $trayTarget, $daemonTarget | Select-Object FullName, Length, LastWriteTime
}
finally {
    Remove-Item -LiteralPath $trayNext, $daemonNext -Force -ErrorAction SilentlyContinue
}
