<#
.SYNOPSIS
  Manage the proven Qwen3.6 27B 64K llama.cpp profile on the reference RTX 4060 Ti.

.DESCRIPTION
  Starts llama-server as a detached hidden process and waits for its health endpoint.
  Re-running Start is safe: it reports an already-healthy matching process instead of
  launching a duplicate. Stop refuses to kill an unrelated process that happens to own
  the configured port.

  Proven profile (2026-07-26):
    context:        65,536
    KV cache:       K=q4_0, V=q4_0
    GPU layers:     99
    flash attention:on
    parallel slots: 1 (required for MTP)
    speculation:    draft-mtp, max draft 2
    reasoning:       enabled, unrestricted
    bind:           0.0.0.0:8080 (all interfaces; Windows Firewall allows inbound
                    llama-server TCP only on Private-profile networks, which covers
                    the trusted LAN and the Tailscale adapter, and blocks Public)

  Live verification on the reference RTX 4060 Ti 16 GB:
    2,048-token coding run: 37.27 server tokens/s, 36.50 end-to-end tokens/s
    MTP acceptance: 89.8%
    observed idle free VRAM after load: approximately 168-543 MiB

  The launcher reduces avoidable OOM risk by refusing a fresh load when less than
  15,000 MiB is free and by refusing duplicate/foreign port ownership. It cannot
  guarantee against another application consuming GPU memory after startup.

  Reasoning is intentionally unrestricted. Completion is still bounded by the API
  request's max_tokens and by the 65,536-token context shared by prompt, reasoning,
  and final answer. If reasoning consumes the remaining context, no launcher flag can
  create additional room for a final answer.

.PARAMETER Action
  Start (default), Status, Restart, or Stop.

.PARAMETER HostAddress
  Address llama-server binds to. Defaults to 0.0.0.0 (all interfaces) so LAN and
  Tailscale clients can reach the server; Windows Firewall still scopes inbound
  access to Private-profile networks. Pass the LAN address for a LAN-only bind.

.PARAMETER Port
  Direct llama.cpp API port. Defaults to 8080.

.PARAMETER ReasoningBudget
  Maximum thinking tokens before llama.cpp closes the reasoning section and asks the
  model for its final answer. Defaults to -1 (unrestricted). Set a positive value only
  when an explicit reasoning cap is desired.

.EXAMPLE
  ./scripts/run-qwen-64k-q4q4.ps1

.EXAMPLE
  ./scripts/run-qwen-64k-q4q4.ps1 Status

.EXAMPLE
  ./scripts/run-qwen-64k-q4q4.ps1 Restart

.EXAMPLE
  ./scripts/run-qwen-64k-q4q4.ps1 Stop
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('Start', 'Status', 'Restart', 'Stop')]
    [string]$Action = 'Start',

    [string]$HostAddress = '0.0.0.0',

    [ValidateRange(1, 65535)]
    [int]$Port = 8080,

    [ValidateRange(-1, 65536)]
    [int]$ReasoningBudget = -1
)

$ErrorActionPreference = 'Stop'

# 0.0.0.0 is a bind address, not a connect target on Windows; probe via loopback.
$connectAddress = if ($HostAddress -eq '0.0.0.0') { '127.0.0.1' } else { $HostAddress }

$repoRoot = Split-Path -Parent $PSScriptRoot
$serverPath = Join-Path $repoRoot 'tools\llama.cpp\llama-server.exe'
$modelPath = Join-Path $repoRoot 'models\Qwen3.6-27B-16GB-VRAM-MTP-mini-IQ4_XS.gguf'
$chatTemplatePath = Join-Path $repoRoot 'configs\qwen3.6-agent-tools.jinja'
$logDirectory = Join-Path $repoRoot 'logs'
$runtimeEventLog = Join-Path $logDirectory 'qwen-runtime-events.jsonl'
$minimumFreeBeforeStartMiB = 15000
$reasoningBudgetMessage = 'Reasoning budget reached. Stop analyzing and provide the final answer now.'

New-Item -ItemType Directory -Force -Path $logDirectory | Out-Null

function Write-RuntimeEvent {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Event,

        [ValidateSet('info', 'warning', 'error')]
        [string]$Level = 'info',

        [object]$Details = @{}
    )

    $entry = [ordered]@{
        timestamp_utc    = (Get-Date).ToUniversalTime().ToString('o')
        event            = $Event
        level            = $Level
        launcher_action  = $Action
        host             = $HostAddress
        port             = $Port
        reasoning_budget = $ReasoningBudget
        details          = $Details
    }
    ($entry | ConvertTo-Json -Depth 8 -Compress) |
        Add-Content -LiteralPath $runtimeEventLog -Encoding UTF8
}

function Get-PortListener {
    Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue |
        Select-Object -First 1
}

function Get-ListenerProcess {
    $listener = Get-PortListener
    if (-not $listener) {
        return $null
    }

    Get-CimInstance Win32_Process -Filter "ProcessId=$($listener.OwningProcess)" -ErrorAction SilentlyContinue
}

function Test-IsExpectedServer {
    param([object]$Process)

    if (-not (Test-IsManagedQwenServer $Process)) {
        return $false
    }

    $commandLine = [string]$Process.CommandLine
    return (
        $commandLine -like "*$modelPath*" -and
        $commandLine -like '*-c 65536*' -and
        $commandLine -like '*--cache-type-k q4_0*' -and
        $commandLine -like '*--cache-type-v q4_0*' -and
        $commandLine -like '*--spec-type draft-mtp*' -and
        $commandLine -like '*--spec-draft-n-max 2*' -and
        $commandLine -like '*--reasoning on*' -and
        $commandLine -like "*--reasoning-budget $ReasoningBudget*" -and
        $commandLine -like "*--chat-template-file `"$chatTemplatePath`"*" -and
        $commandLine -like "*--host $HostAddress*" -and
        $commandLine -like "*--port $Port*"
    )
}

function Test-IsManagedQwenServer {
    param([object]$Process)

    if (-not $Process -or $Process.Name -ne 'llama-server.exe') {
        return $false
    }

    $commandLine = [string]$Process.CommandLine
    return (
        $commandLine -like "*$modelPath*" -and
        $commandLine -like "*--host $HostAddress*" -and
        $commandLine -like "*--port $Port*"
    )
}

function Get-Health {
    try {
        Invoke-RestMethod -Uri "http://${connectAddress}:$Port/health" -TimeoutSec 3
    }
    catch {
        return $null
    }
}

function Show-Status {
    $listener = Get-PortListener
    if (-not $listener) {
        Write-RuntimeEvent -Event 'status_stopped'
        Write-Host "Stopped: nothing is listening on ${HostAddress}:$Port."
        return
    }

    $process = Get-ListenerProcess
    $matches = Test-IsExpectedServer $process
    $health = Get-Health
    Write-RuntimeEvent -Event 'status_observed' -Details @{
        pid = $listener.OwningProcess
        address = "$($listener.LocalAddress):$($listener.LocalPort)"
        expected_config = $matches
        health = if ($health) { $health.status } else { 'unreachable' }
    }

    [pscustomobject]@{
        PID            = $listener.OwningProcess
        Address        = "$($listener.LocalAddress):$($listener.LocalPort)"
        ExpectedConfig = $matches
        Health         = if ($health) { $health.status } else { 'unreachable' }
        CommandLine    = $process.CommandLine
    } | Format-List

    if (Get-Command nvidia-smi -ErrorAction SilentlyContinue) {
        Write-Host 'GPU memory:'
        nvidia-smi --query-gpu=memory.total,memory.used,memory.free --format=csv,noheader
    }
}

function Stop-Server {
    $listener = Get-PortListener
    if (-not $listener) {
        Write-RuntimeEvent -Event 'stop_already_stopped'
        Write-Host "Already stopped: port $Port is free."
        return
    }

    $process = Get-ListenerProcess
    if (-not (Test-IsManagedQwenServer $process)) {
        Write-RuntimeEvent -Event 'stop_refused_foreign_process' -Level 'error' -Details @{
            pid = $listener.OwningProcess
            command_line = $process.CommandLine
        }
        throw "Refusing to stop PID $($listener.OwningProcess): it is not this launcher's Qwen server."
    }

    Write-RuntimeEvent -Event 'stop_requested' -Details @{
        pid = $listener.OwningProcess
        command_line = $process.CommandLine
    }
    Write-Host "Stopping Qwen llama-server PID $($listener.OwningProcess)..."
    Stop-Process -Id $listener.OwningProcess -Force
    Wait-Process -Id $listener.OwningProcess -Timeout 30 -ErrorAction SilentlyContinue

    $releaseDeadline = (Get-Date).AddSeconds(30)
    while ((Get-PortListener) -and (Get-Date) -lt $releaseDeadline) {
        Start-Sleep -Milliseconds 250
    }

    if (Get-PortListener) {
        Write-RuntimeEvent -Event 'stop_port_release_timeout' -Level 'error' -Details @{
            pid = $listener.OwningProcess
        }
        throw "PID $($listener.OwningProcess) stopped, but port $Port is still occupied."
    }

    Write-RuntimeEvent -Event 'stop_completed' -Details @{
        pid = $listener.OwningProcess
    }
    Write-Host 'Stopped.' -ForegroundColor Green
}

function Get-FreeVramMiB {
    if (-not (Get-Command nvidia-smi -ErrorAction SilentlyContinue)) {
        throw 'nvidia-smi is required for the conservative VRAM preflight.'
    }

    $nvidiaOutput = @(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits)
    $nvidiaExitCode = $LASTEXITCODE
    $raw = $nvidiaOutput | Select-Object -First 1
    $number = [regex]::Match([string]$raw, '\d+')
    if ($nvidiaExitCode -ne 0 -or -not $number.Success) {
        throw 'Could not read free VRAM from nvidia-smi.'
    }

    [int]$number.Value
}

function Start-Server {
    $existing = Get-PortListener
    if ($existing) {
        $process = Get-ListenerProcess
        if ((Test-IsExpectedServer $process) -and (Get-Health)) {
            Write-RuntimeEvent -Event 'start_already_healthy' -Details @{
                pid = $existing.OwningProcess
            }
            Write-Host "Already healthy: PID $($existing.OwningProcess) at http://${connectAddress}:$Port." -ForegroundColor Green
            return
        }

        Write-RuntimeEvent -Event 'start_refused_occupied_port' -Level 'error' -Details @{
            pid = $existing.OwningProcess
            command_line = $process.CommandLine
        }
        throw "Port $Port is already occupied by PID $($existing.OwningProcess); refusing a duplicate or unsafe replacement."
    }

    if (-not (Test-Path -LiteralPath $serverPath -PathType Leaf)) {
        throw "llama-server not found: $serverPath"
    }
    if (-not (Test-Path -LiteralPath $modelPath -PathType Leaf)) {
        throw "Model not found: $modelPath"
    }

    $freeVramMiB = Get-FreeVramMiB
    Write-RuntimeEvent -Event 'vram_preflight_observed' -Details @{
        free_mib = $freeVramMiB
        required_mib = $minimumFreeBeforeStartMiB
    }
    if ($freeVramMiB -lt $minimumFreeBeforeStartMiB) {
        Write-RuntimeEvent -Event 'vram_preflight_refused' -Level 'error' -Details @{
            free_mib = $freeVramMiB
            required_mib = $minimumFreeBeforeStartMiB
        }
        throw "Refusing fresh load: $freeVramMiB MiB VRAM is free; this profile requires at least $minimumFreeBeforeStartMiB MiB before startup."
    }

    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $stdoutLog = Join-Path $logDirectory "qwen-64k-q4q4-$stamp.out.log"
    $stderrLog = Join-Path $logDirectory "qwen-64k-q4q4-$stamp.err.log"
    $engineLog = Join-Path $logDirectory "qwen-64k-q4q4-$stamp.engine.log"

    $arguments = @(
        '-m', "`"$modelPath`"",
        '-ngl', '99',
        '-c', '65536',
        '-fa', 'on',
        '-np', '1',
        '--cache-type-k', 'q4_0',
        '--cache-type-v', 'q4_0',
        '--spec-type', 'draft-mtp',
        '--spec-draft-n-max', '2',
        '--reasoning', 'on',
        '--reasoning-budget', [string]$ReasoningBudget,
        '--chat-template-file', "`"$chatTemplatePath`"",
        '-lv', '3',
        '--log-prefix',
        '--log-timestamps',
        '--log-file', "`"$engineLog`"",
        '--metrics',
        '--slots',
        '--host', $HostAddress,
        '--port', [string]$Port
    )
    if ($ReasoningBudget -ge 0) {
        $hostArgumentIndex = [array]::IndexOf($arguments, '--host')
        $arguments = @(
            $arguments[0..($hostArgumentIndex - 1)]
            '--reasoning-budget-message'
            "`"$reasoningBudgetMessage`""
            $arguments[$hostArgumentIndex..($arguments.Count - 1)]
        )
    }

    Write-Host "Starting the documented 64K q4/q4 profile with $freeVramMiB MiB free..."
    Write-RuntimeEvent -Event 'start_requested' -Details @{
        free_vram_mib = $freeVramMiB
        server_path = $serverPath
        model_path = $modelPath
        arguments = $arguments
        stdout_log = $stdoutLog
        stderr_log = $stderrLog
        engine_log = $engineLog
    }
    $process = Start-Process `
        -FilePath $serverPath `
        -ArgumentList $arguments `
        -WindowStyle Hidden `
        -RedirectStandardOutput $stdoutLog `
        -RedirectStandardError $stderrLog `
        -PassThru

    Write-RuntimeEvent -Event 'process_spawned' -Details @{
        pid = $process.Id
    }
    $deadline = (Get-Date).AddMinutes(3)
    do {
        Start-Sleep -Seconds 2
        if ($process.HasExited) {
            $tail = if (Test-Path -LiteralPath $stderrLog) {
                (Get-Content -LiteralPath $stderrLog -Tail 30) -join [Environment]::NewLine
            }
            else {
                '(no error log)'
            }
            Write-RuntimeEvent -Event 'startup_process_exited' -Level 'error' -Details @{
                pid = $process.Id
                exit_code = $process.ExitCode
                error_tail = $tail
            }
            throw "llama-server exited during startup (code $($process.ExitCode)).`n$tail"
        }
        $health = Get-Health
    } while (-not $health -and (Get-Date) -lt $deadline)

    if (-not $health) {
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        Write-RuntimeEvent -Event 'startup_health_timeout' -Level 'error' -Details @{
            pid = $process.Id
            stderr_log = $stderrLog
        }
        throw "Health check timed out after 3 minutes. See $stderrLog"
    }

    Write-RuntimeEvent -Event 'startup_healthy' -Details @{
        pid = $process.Id
        endpoint = "http://${connectAddress}:$Port"
        stdout_log = $stdoutLog
        stderr_log = $stderrLog
        engine_log = $engineLog
    }
    Write-Host "Healthy: PID $($process.Id), http://${connectAddress}:$Port" -ForegroundColor Green
    Write-Host "Logs: $engineLog, $stdoutLog, $stderrLog, and $runtimeEventLog"
}

Write-RuntimeEvent -Event 'launcher_action_requested'
try {
    switch ($Action) {
        'Start' {
            Start-Server
            Show-Status
        }
        'Status' {
            Show-Status
        }
        'Restart' {
            Stop-Server
            Start-Server
            Show-Status
        }
        'Stop' {
            Stop-Server
        }
    }
}
catch {
    Write-RuntimeEvent -Event 'launcher_action_failed' -Level 'error' -Details @{
        error = $_.Exception.Message
        script_stack = $_.ScriptStackTrace
    }
    throw
}
