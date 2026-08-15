[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('Install', 'Uninstall', 'Start', 'Stop', 'Status')]
    [string]$Action = 'Status'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$trayExe = Join-Path $repoRoot 'localhub-tray.exe'
$taskName = 'LocalHub-Tray'
$legacyQwenTaskName = 'LocalHub-Qwen3.6-KeepAvailable'
$runKeyPath = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$runValueName = 'LocalHub Compass'
$startupApprovedKeyPath = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run'

function Get-TrayProcess {
    Get-CimInstance Win32_Process -Filter "Name='localhub-tray.exe'" -ErrorAction SilentlyContinue |
        Where-Object { [string]$_.ExecutablePath -eq $trayExe }
}

function Get-StartupCommand {
    return "`"$trayExe`" -repo `"$repoRoot`""
}

function Test-StartupEnabled {
    $item = Get-ItemProperty -LiteralPath $runKeyPath -ErrorAction SilentlyContinue
    $property = if ($item) { $item.PSObject.Properties[$runValueName] } else { $null }
    $value = if ($property) { $property.Value } else { $null }
    if ([string]$value -ne (Get-StartupCommand)) {
        return $false
    }
    $approvedItem = Get-ItemProperty -LiteralPath $startupApprovedKeyPath -ErrorAction SilentlyContinue
    $approvedProperty = if ($approvedItem) { $approvedItem.PSObject.Properties[$runValueName] } else { $null }
    $approved = if ($approvedProperty) { [byte[]]$approvedProperty.Value } else { $null }
    return -not ($approved -and $approved.Count -gt 0 -and $approved[0] -eq 3)
}

function Enable-Startup {
    if (-not (Test-Path -LiteralPath $trayExe -PathType Leaf)) {
        & (Join-Path $PSScriptRoot 'build-localhub-tray.ps1')
    }

    # Migrate the older Scheduled Task registration. HKCU Run is the native Windows Startup Apps
    # surface, so LocalHub can also be disabled from Task Manager without leaving a second launcher.
    $existing = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if ($existing) {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    }
    $legacyQwenTask = Get-ScheduledTask -TaskName $legacyQwenTaskName -ErrorAction SilentlyContinue
    if ($legacyQwenTask) {
        Stop-ScheduledTask -TaskName $legacyQwenTaskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $legacyQwenTaskName -Confirm:$false
    }
    New-Item -Path $runKeyPath -Force | Out-Null
    New-ItemProperty -LiteralPath $runKeyPath -Name $runValueName `
        -PropertyType String -Value (Get-StartupCommand) -Force | Out-Null
    Remove-ItemProperty -LiteralPath $startupApprovedKeyPath -Name $runValueName -ErrorAction SilentlyContinue
}

function Start-Tray {
    if (-not (Get-TrayProcess)) {
        Start-Process -FilePath $trayExe `
            -ArgumentList @('-repo', "`"$repoRoot`"") `
            -WorkingDirectory $repoRoot `
            -WindowStyle Hidden
    }
}

switch ($Action) {
    'Install' {
        Enable-Startup
        Start-Tray
    }
    'Uninstall' {
        Remove-ItemProperty -LiteralPath $runKeyPath -Name $runValueName -ErrorAction SilentlyContinue
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
        Get-TrayProcess | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
    }
    'Start' {
        if (-not (Test-Path -LiteralPath $trayExe -PathType Leaf)) {
            throw "Tray executable not found. Run: $PSCommandPath Install"
        }
        Start-Tray
    }
    'Stop' {
        Get-TrayProcess | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
    }
}

$process = Get-TrayProcess | Select-Object -First 1
[pscustomobject]@{
    Installed = Test-StartupEnabled
    StartupRegistration = if (Test-StartupEnabled) { 'Windows Startup Apps' } else { 'Disabled' }
    Running = [bool]$process
    PID = if ($process) { $process.ProcessId } else { $null }
    Executable = $trayExe
} | Format-List
