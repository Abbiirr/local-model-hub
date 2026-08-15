<#
.SYNOPSIS
  Keep LocalHub and the proven Qwen3.6 64K runtime available on this workstation.

.DESCRIPTION
  LocalHub supervises engine health, but it cannot restart itself and currently records an
  unexpected post-HEALTHY engine exit as FAILED. This script supplies the outer desired-state
  loop:

    * LocalHub is kept running.
    * While Qwen is desired UP, the canonical run-qwen-64k-q4q4.ps1 launcher is kept healthy.
    * The live Qwen process is attached to LocalHub so /v1 routes through LocalHub.
    * A clean LocalHub STOPPED state or a direct launcher "Stop" is treated as an intentional
      operator down and disables Qwen recovery.
    * A crash, hard kill, FAILED state, or machine/logon restart is recovered.

  Use Up and Down for the durable operator intent. Install creates one long-running,
  current-user logon task. The Watch action reacts to LocalHub/llama-server process
  start and exit events immediately, with a low-frequency health fallback for hangs.
  A separate low-frequency task trigger rescues the Watch process itself if it is killed.
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('Ensure', 'Watch', 'StartHub', 'StartHubOnce', 'Up', 'Down', 'DownAll', 'Status', 'Install', 'Uninstall')]
    [string]$Action = 'Status',

    [Parameter(Position = 1)]
    [ValidateRange(1, 1440)]
    [int]$EnsureIntervalMinutes = 5,

    [ValidateRange(1, 1440)]
    [int]$RescueIntervalMinutes = 15
)

$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$localHubExe = Join-Path $repoRoot 'localhub.exe'
$localHubConfig = Join-Path $repoRoot 'configs\config.yaml'
$qwenLauncher = Join-Path $PSScriptRoot 'run-qwen-64k-q4q4.ps1'
$runtimeDirectory = Join-Path $repoRoot 'logs'
$desiredUpPath = Join-Path $runtimeDirectory 'qwen-localhub.desired-up.json'
$supervisorStatePath = Join-Path $runtimeDirectory 'qwen-localhub.supervisor-state.json'
$supervisorLogPath = Join-Path $runtimeDirectory 'qwen-localhub-supervisor.jsonl'
$qwenRuntimeLogPath = Join-Path $runtimeDirectory 'qwen-runtime-events.jsonl'
$taskName = 'LocalHub-Qwen3.6-KeepAvailable'
$localHubPort = 39876
$qwenPort = 8080
$localHubBaseURL = "http://127.0.0.1:$localHubPort"
$qwenHealthURL = "http://127.0.0.1:$qwenPort/health"
$expectedModelPath = Join-Path $repoRoot 'models\Qwen3.6-27B-16GB-VRAM-MTP-mini-IQ4_XS.gguf'

New-Item -ItemType Directory -Force -Path $runtimeDirectory | Out-Null

function Write-SupervisorEvent {
    param(
        [Parameter(Mandatory = $true)][string]$Event,
        [ValidateSet('info', 'warning', 'error')][string]$Level = 'info',
        [object]$Details = @{}
    )
    $entry = [ordered]@{
        timestamp_utc = (Get-Date).ToUniversalTime().ToString('o')
        event = $Event
        level = $Level
        action = $Action
        details = $Details
    }
    ($entry | ConvertTo-Json -Depth 8 -Compress) |
        Add-Content -LiteralPath $supervisorLogPath -Encoding UTF8
}

function Read-JSONFile {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return $null
    }
    try {
        return Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
    }
    catch {
        Write-SupervisorEvent -Event 'state_file_invalid' -Level 'warning' -Details @{
            path = $Path
            error = $_.Exception.Message
        }
        return $null
    }
}

function Write-JSONFile {
    param([string]$Path, [object]$Value)
    $temporaryPath = "$Path.tmp"
    $Value | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $temporaryPath -Encoding UTF8
    Move-Item -LiteralPath $temporaryPath -Destination $Path -Force
}

function Get-Listener {
    param([int]$Port)
    Get-NetTCPConnection -State Listen -LocalPort $Port -ErrorAction SilentlyContinue |
        Select-Object -First 1
}

function Get-ListenerProcess {
    param([int]$Port)
    $listener = Get-Listener -Port $Port
    if (-not $listener) {
        return $null
    }
    Get-CimInstance Win32_Process -Filter "ProcessId=$($listener.OwningProcess)" -ErrorAction SilentlyContinue
}

function Test-LocalHubHealth {
    try {
        $health = Invoke-RestMethod -Uri "$localHubBaseURL/api/health" -TimeoutSec 3
        return $health.status -eq 'ok'
    }
    catch {
        return $false
    }
}

function Test-QwenHealth {
    try {
        $health = Invoke-RestMethod -Uri $qwenHealthURL -TimeoutSec 3
        return $health.status -eq 'ok'
    }
    catch {
        return $false
    }
}

function Test-ExpectedLocalHubProcess {
    param([object]$Process)
    if (-not $Process -or $Process.Name -ne 'localhub.exe') {
        return $false
    }
    $commandLine = [string]$Process.CommandLine
    return $commandLine -like '*-config*configs/config.yaml*' -or
        $commandLine -like '*-config*configs\config.yaml*'
}

function Test-ExpectedQwenProcess {
    param([object]$Process)
    if (-not $Process -or $Process.Name -ne 'llama-server.exe') {
        return $false
    }
    $commandLine = [string]$Process.CommandLine
    return $commandLine -like "*$expectedModelPath*" -and
        $commandLine -like '*-c 65536*' -and
        $commandLine -like '*--cache-type-k q4_0*' -and
        $commandLine -like '*--cache-type-v q4_0*' -and
        $commandLine -like '*--spec-type draft-mtp*' -and
        $commandLine -like '*--spec-draft-n-max 2*'
}

function Get-SupervisorState {
    $state = Read-JSONFile -Path $supervisorStatePath
    if (-not $state) {
        $state = [pscustomobject]@{
            localhub_health_failures = 0
            qwen_health_failures = 0
            qwen_last_pid = 0
        }
    }
    if ($state.PSObject.Properties.Name -notcontains 'qwen_last_pid') {
        $state | Add-Member -NotePropertyName qwen_last_pid -NotePropertyValue 0
    }
    return $state
}

function Save-SupervisorState {
    param([object]$State)
    Write-JSONFile -Path $supervisorStatePath -Value $State
}

function Start-LocalHub {
    if (Test-LocalHubHealth) {
        return
    }
    if (-not (Test-Path -LiteralPath $localHubExe -PathType Leaf)) {
        throw "LocalHub executable not found: $localHubExe"
    }
    if (-not (Test-Path -LiteralPath $localHubConfig -PathType Leaf)) {
        throw "LocalHub config not found: $localHubConfig"
    }
    $listener = Get-Listener -Port $localHubPort
    if ($listener) {
        throw "Port $localHubPort is occupied by PID $($listener.OwningProcess), but LocalHub health is unavailable."
    }

    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $stdoutLog = Join-Path $runtimeDirectory "localhub-$stamp.out.log"
    $stderrLog = Join-Path $runtimeDirectory "localhub-$stamp.err.log"
    $process = Start-Process -FilePath $localHubExe `
        -ArgumentList @('-config', 'configs/config.yaml') `
        -WorkingDirectory $repoRoot `
        -WindowStyle Hidden `
        -RedirectStandardOutput $stdoutLog `
        -RedirectStandardError $stderrLog `
        -PassThru
    Write-SupervisorEvent -Event 'localhub_started' -Details @{
        pid = $process.Id
        stdout_log = $stdoutLog
        stderr_log = $stderrLog
    }

    $deadline = (Get-Date).AddSeconds(45)
    do {
        Start-Sleep -Milliseconds 500
        if ($process.HasExited) {
            $tail = if (Test-Path -LiteralPath $stderrLog) {
                (Get-Content -LiteralPath $stderrLog -Tail 30) -join [Environment]::NewLine
            }
            else {
                '(no stderr log)'
            }
            throw "LocalHub exited during startup (code $($process.ExitCode)). $tail"
        }
    } while (-not (Test-LocalHubHealth) -and (Get-Date) -lt $deadline)

    if (-not (Test-LocalHubHealth)) {
        throw "LocalHub did not become healthy within 45 seconds. See $stderrLog"
    }
}

function Ensure-LocalHub {
    param([object]$State)
    if (Test-LocalHubHealth) {
        $State.localhub_health_failures = 0
        return
    }

    $process = Get-ListenerProcess -Port $localHubPort
    if ($process) {
        if (-not (Test-ExpectedLocalHubProcess -Process $process)) {
            throw "Refusing recovery: port $localHubPort belongs to unexpected process PID $($process.ProcessId)."
        }
        $State.localhub_health_failures = [int]$State.localhub_health_failures + 1
        if ($State.localhub_health_failures -lt 3) {
            Write-SupervisorEvent -Event 'localhub_health_degraded' -Level 'warning' -Details @{
                pid = $process.ProcessId
                consecutive_failures = $State.localhub_health_failures
                reap_after = 3
            }
            return
        }
        Write-SupervisorEvent -Event 'localhub_unresponsive_reaped' -Level 'warning' -Details @{
            pid = $process.ProcessId
            consecutive_failures = $State.localhub_health_failures
        }
        Stop-Process -Id $process.ProcessId -Force
        Wait-Process -Id $process.ProcessId -Timeout 30 -ErrorAction SilentlyContinue
        $State.localhub_health_failures = 0
    }
    Start-LocalHub
}

function Get-DesiredUp {
    Read-JSONFile -Path $desiredUpPath
}

function Set-DesiredUp {
    $desired = [ordered]@{
        desired = 'up'
        requested_at_utc = (Get-Date).ToUniversalTime().ToString('o')
        requested_by = [Environment]::UserName
    }
    Write-JSONFile -Path $desiredUpPath -Value $desired
    Write-SupervisorEvent -Event 'desired_state_up'
}

function Set-DesiredDown {
    param([string]$Reason)
    if (Test-Path -LiteralPath $desiredUpPath) {
        Remove-Item -LiteralPath $desiredUpPath -Force
    }
    Write-SupervisorEvent -Event 'desired_state_down' -Details @{ reason = $Reason }
}

function Get-LocalHubInstances {
    try {
        # Windows PowerShell 5 returns a JSON top-level array as one Object[] pipeline item.
        # Emit each row explicitly so callers never receive a nested array.
        $response = Invoke-RestMethod -Uri "$localHubBaseURL/api/instances" -TimeoutSec 5
        foreach ($instance in $response) {
            Write-Output $instance
        }
    }
    catch {
        return
    }
}

function Get-QwenInstances {
    Get-LocalHubInstances | Where-Object {
        $_.model_path -eq $expectedModelPath -or
        ([string]$_.model_path).EndsWith('Qwen3.6-27B-16GB-VRAM-MTP-mini-IQ4_XS.gguf')
    }
}

function Test-IntentionalDownAfter {
    param([datetime]$DesiredSince)

    # A clean LocalHub STOPPED transition is an operator stop. FAILED means recovery is wanted.
    $latest = Get-QwenInstances |
        Sort-Object { if ($_.created_at) { [datetime]$_.created_at } else { [datetime]::MinValue } } -Descending |
        Select-Object -First 1
    $cleanStop = $latest -and
        $latest.state -eq 'STOPPED' -and
        $latest.stopped_at -and
        ([string]$latest.reason) -notlike 'process gone at reconcile*'
    if ($cleanStop) {
        $stoppedAt = ([datetime]$latest.stopped_at).ToUniversalTime()
        if ($stoppedAt -gt $DesiredSince.ToUniversalTime()) {
            return "LocalHub recorded a clean STOPPED transition at $($stoppedAt.ToString('o'))."
        }
    }

    # The canonical launcher's explicit Stop action is also operator intent. Restart emits its
    # own stop event with launcher_action=Restart, so it does not disable recovery.
    if (Test-Path -LiteralPath $qwenRuntimeLogPath -PathType Leaf) {
        $events = Get-Content -LiteralPath $qwenRuntimeLogPath -Tail 300 -ErrorAction SilentlyContinue
        foreach ($line in $events) {
            try {
                $event = $line | ConvertFrom-Json
                if ($event.event -eq 'stop_requested' -and
                    $event.launcher_action -eq 'Stop' -and
                    ([datetime]$event.timestamp_utc).ToUniversalTime() -gt $DesiredSince.ToUniversalTime()) {
                    return "The canonical Qwen launcher recorded an explicit Stop action at $($event.timestamp_utc)."
                }
            }
            catch {
                continue
            }
        }
    }
    return $null
}

function Ensure-Qwen {
    param([object]$State)
    $desired = Get-DesiredUp
    if (-not $desired) {
        return
    }
    $desiredSince = ([datetime]$desired.requested_at_utc).ToUniversalTime()
    $intentionalDown = Test-IntentionalDownAfter -DesiredSince $desiredSince
    if ($intentionalDown) {
        Set-DesiredDown -Reason $intentionalDown
        return
    }

    $process = Get-ListenerProcess -Port $qwenPort
    if ($process -and -not (Test-ExpectedQwenProcess -Process $process)) {
        throw "Refusing recovery: port $qwenPort belongs to unexpected process PID $($process.ProcessId)."
    }

    if ($process -and -not (Test-QwenHealth)) {
        if ([int]$State.qwen_last_pid -ne [int]$process.ProcessId) {
            $State.qwen_last_pid = [int]$process.ProcessId
            $State.qwen_health_failures = 0
        }
        $nativeProcess = Get-Process -Id $process.ProcessId -ErrorAction SilentlyContinue
        $processAge = if ($nativeProcess) { (Get-Date) - $nativeProcess.StartTime } else { [timespan]::MaxValue }
        # A cold 27B load on the reference box has repeatedly taken about 2m30s. The canonical
        # launcher owns a three-minute health deadline, so the health fallback must not mistake
        # a normal cold load for three consecutive post-start health failures and kill it just
        # before readiness.
        if ($processAge -lt (New-TimeSpan -Minutes 4)) {
            Write-SupervisorEvent -Event 'qwen_starting' -Details @{
                pid = $process.ProcessId
                age_seconds = [math]::Round($processAge.TotalSeconds, 1)
                startup_grace_seconds = 240
            }
            return
        }
        $State.qwen_health_failures = [int]$State.qwen_health_failures + 1
        if ($State.qwen_health_failures -lt 3) {
            Write-SupervisorEvent -Event 'qwen_health_degraded' -Level 'warning' -Details @{
                pid = $process.ProcessId
                consecutive_failures = $State.qwen_health_failures
                reap_after = 3
            }
            return
        }
        Write-SupervisorEvent -Event 'qwen_unresponsive_reaped' -Level 'warning' -Details @{
            pid = $process.ProcessId
            consecutive_failures = $State.qwen_health_failures
        }
        Stop-Process -Id $process.ProcessId -Force
        Wait-Process -Id $process.ProcessId -Timeout 30 -ErrorAction SilentlyContinue
        $State.qwen_health_failures = 0
        $process = $null
    }

    if (-not $process -or -not (Test-QwenHealth)) {
        Write-SupervisorEvent -Event 'qwen_recovery_started'
        & $qwenLauncher Start
        if (-not (Test-QwenHealth)) {
            throw 'The Qwen launcher returned, but Qwen health is unavailable.'
        }
        Write-SupervisorEvent -Event 'qwen_recovery_healthy'
    }
    else {
        $State.qwen_health_failures = 0
        if ($process) {
            $State.qwen_last_pid = [int]$process.ProcessId
        }
    }

    # Attach is idempotent from the supervisor's perspective: HTTP 409 "already tracked" means
    # LocalHub already owns the lifecycle row. A new PID after failure gets a new attached row.
    $healthyTracked = Get-QwenInstances | Where-Object {
        $_.state -eq 'HEALTHY' -and $_.pid -eq (Get-Listener -Port $qwenPort).OwningProcess
    } | Select-Object -First 1
    if (-not $healthyTracked) {
        $body = @{
            port = $qwenPort
            host = '127.0.0.1'
            pin = $true
        } | ConvertTo-Json
        try {
            $attached = Invoke-RestMethod -Method Post `
                -Uri "$localHubBaseURL/api/instances/attach" `
                -ContentType 'application/json' `
                -Body $body `
                -TimeoutSec 30
            Write-SupervisorEvent -Event 'qwen_attached_to_localhub' -Details @{
                instance_id = $attached.id
                pid = $attached.pid
                model_id = $attached.model_id
            }
        }
        catch {
            # An old HEALTHY row can briefly own the dead PID until LocalHub's 15 s monitor pass.
            # Leave the engine healthy and retry attach on the next scheduled Ensure.
            Write-SupervisorEvent -Event 'qwen_attach_deferred' -Level 'warning' -Details @{
                error = $_.Exception.Message
            }
        }
    }
}

function Invoke-Ensure {
    $state = Get-SupervisorState
    try {
        Ensure-LocalHub -State $state
        Ensure-Qwen -State $state
        Write-SupervisorEvent -Event 'ensure_completed' -Details @{
            localhub_healthy = (Test-LocalHubHealth)
            qwen_desired = [bool](Get-DesiredUp)
            qwen_healthy = (Test-QwenHealth)
        }
    }
    catch {
        Write-SupervisorEvent -Event 'ensure_failed' -Level 'error' -Details @{
            error = $_.Exception.Message
            script_stack = $_.ScriptStackTrace
        }
        throw
    }
    finally {
        Save-SupervisorState -State $state
    }
}

function Invoke-SerializedEnsure {
    $ensureMutex = New-Object System.Threading.Mutex($false, 'LocalHubQwenSupervisor')
    $ownsMutex = $false
    try {
        try {
            $ownsMutex = $ensureMutex.WaitOne(30000)
        }
        catch [System.Threading.AbandonedMutexException] {
            $ownsMutex = $true
        }
        if (-not $ownsMutex) {
            Write-SupervisorEvent -Event 'watch_ensure_deferred' -Level 'warning' -Details @{
                reason = 'another supervisor action held the mutex for more than 30 seconds'
            }
            return
        }
        Invoke-Ensure
    }
    finally {
        if ($ownsMutex) {
            $ensureMutex.ReleaseMutex()
        }
        $ensureMutex.Dispose()
    }
}

function Invoke-Watch {
    $processWatches = @{}
    $desiredWatcher = $null
    $sourceIdentifiers = @(
        'LocalHubQwenWatch.LocalHub.Exited',
        'LocalHubQwenWatch.Qwen.Exited',
        'LocalHubQwenWatch.Desired.Created',
        'LocalHubQwenWatch.Desired.Changed',
        'LocalHubQwenWatch.Desired.Deleted'
    )
    $consecutiveFailures = 0

    function Clear-ProcessWatch {
        param([string]$Role)
        if (-not $processWatches.ContainsKey($Role)) {
            return
        }
        $watch = $processWatches[$Role]
        Unregister-Event -SourceIdentifier $watch.SourceIdentifier -ErrorAction SilentlyContinue
        Get-Event -SourceIdentifier $watch.SourceIdentifier -ErrorAction SilentlyContinue |
            Remove-Event -ErrorAction SilentlyContinue
        $watch.Process.Dispose()
        $processWatches.Remove($Role)
    }

    function Set-ProcessWatch {
        param(
            [string]$Role,
            [int]$Port,
            [string]$SourceIdentifier
        )
        $listener = Get-Listener -Port $Port
        $pidToWatch = if ($listener) { [int]$listener.OwningProcess } else { 0 }
        if ($processWatches.ContainsKey($Role) -and
            [int]$processWatches[$Role].PID -eq $pidToWatch) {
            return
        }
        Clear-ProcessWatch -Role $Role
        if (-not $pidToWatch) {
            return
        }
        try {
            $nativeProcess = Get-Process -Id $pidToWatch -ErrorAction Stop
            $nativeProcess.EnableRaisingEvents = $true
            Register-ObjectEvent -InputObject $nativeProcess -EventName Exited `
                -SourceIdentifier $SourceIdentifier | Out-Null
            $processWatches[$Role] = [pscustomobject]@{
                PID = $pidToWatch
                Process = $nativeProcess
                SourceIdentifier = $SourceIdentifier
            }
            Write-SupervisorEvent -Event 'process_exit_watch_registered' -Details @{
                role = $Role
                pid = $pidToWatch
            }
        }
        catch {
            Write-SupervisorEvent -Event 'process_exit_watch_registration_failed' -Level 'warning' -Details @{
                role = $Role
                pid = $pidToWatch
                error = $_.Exception.Message
            }
        }
    }

    function Update-ProcessWatches {
        Set-ProcessWatch -Role 'LocalHub' -Port $localHubPort `
            -SourceIdentifier 'LocalHubQwenWatch.LocalHub.Exited'
        Set-ProcessWatch -Role 'Qwen' -Port $qwenPort `
            -SourceIdentifier 'LocalHubQwenWatch.Qwen.Exited'
    }

    try {
        # Exact Process.Exited subscriptions provide immediate crash detection without WMI
        # privileges. FileSystemWatcher wakes the loop when Up/Down changes durable intent, so
        # a newly launched Qwen process is subscribed without waiting for the health fallback.
        $desiredWatcher = New-Object System.IO.FileSystemWatcher
        $desiredWatcher.Path = $runtimeDirectory
        $desiredWatcher.Filter = (Split-Path -Leaf $desiredUpPath)
        $desiredWatcher.NotifyFilter = [System.IO.NotifyFilters]'FileName, LastWrite'
        foreach ($changeType in @('Created', 'Changed', 'Deleted')) {
            Register-ObjectEvent -InputObject $desiredWatcher -EventName $changeType `
                -SourceIdentifier "LocalHubQwenWatch.Desired.$changeType" | Out-Null
        }
        $desiredWatcher.EnableRaisingEvents = $true

        Write-SupervisorEvent -Event 'watch_started' -Details @{
            health_fallback_minutes = $EnsureIntervalMinutes
            event_sources = @('System.Diagnostics.Process.Exited', 'System.IO.FileSystemWatcher')
        }
        Invoke-SerializedEnsure
        Update-ProcessWatches

        while ($true) {
            $event = Wait-Event -Timeout ([math]::Max(1, $EnsureIntervalMinutes * 60))
            if ($event) {
                Remove-Event -EventIdentifier $event.EventIdentifier -ErrorAction SilentlyContinue
                Write-SupervisorEvent -Event 'process_lifecycle_event' -Details @{
                    source_identifier = $event.SourceIdentifier
                }
                if ($event.SourceIdentifier -like '*.Exited') {
                    Start-Sleep -Milliseconds 750
                }
            }
            else {
                Write-SupervisorEvent -Event 'watch_health_fallback'
            }

            try {
                Invoke-SerializedEnsure
                Update-ProcessWatches
                $consecutiveFailures = 0
            }
            catch {
                $consecutiveFailures++
                Write-SupervisorEvent -Event 'watch_recovery_failed' -Level 'error' -Details @{
                    consecutive_failures = $consecutiveFailures
                    error = $_.Exception.Message
                }
                if ($consecutiveFailures -ge 3) {
                    throw
                }
                Start-Sleep -Seconds ([math]::Min(30, [math]::Pow(2, $consecutiveFailures)))
            }
        }
    }
    finally {
        foreach ($role in @($processWatches.Keys)) {
            Clear-ProcessWatch -Role $role
        }
        foreach ($sourceIdentifier in $sourceIdentifiers) {
            Unregister-Event -SourceIdentifier $sourceIdentifier -ErrorAction SilentlyContinue
            Get-Event -SourceIdentifier $sourceIdentifier -ErrorAction SilentlyContinue |
                Remove-Event -ErrorAction SilentlyContinue
        }
        if ($desiredWatcher) {
            $desiredWatcher.Dispose()
        }
        Write-SupervisorEvent -Event 'watch_stopped'
    }
}

function Invoke-Down {
    Set-DesiredDown -Reason 'operator invoked Down'
    $localHubStopRequested = $false
    if (Test-LocalHubHealth) {
        $active = Get-QwenInstances | Where-Object {
            $_.state -eq 'HEALTHY' -or $_.state -eq 'STARTING' -or $_.state -eq 'DRAINING'
        } | Select-Object -First 1
        if ($active) {
            try {
                Invoke-RestMethod -Method Delete `
                    -Uri "$localHubBaseURL/api/instances/$($active.id)?force=true" `
                    -TimeoutSec 10 | Out-Null
                $localHubStopRequested = $true
                $deadline = (Get-Date).AddSeconds(90)
                while ((Get-Listener -Port $qwenPort) -and (Get-Date) -lt $deadline) {
                    Start-Sleep -Seconds 1
                }
                if (-not (Get-Listener -Port $qwenPort)) {
                    Write-SupervisorEvent -Event 'qwen_down_completed' -Details @{
                        path = 'localhub'
                        instance_id = $active.id
                    }
                    return
                }
            }
            catch {
                Write-SupervisorEvent -Event 'qwen_down_via_localhub_failed' -Level 'warning' -Details @{
                    error = $_.Exception.Message
                }
            }
        }
    }
    if (Get-Listener -Port $qwenPort) {
        & $qwenLauncher Stop
        Write-SupervisorEvent -Event 'qwen_down_completed' -Details @{ path = 'launcher_fallback' }
        return
    }
    $path = if ($localHubStopRequested) { 'localhub_process_already_gone' } else { 'already_down' }
    Write-SupervisorEvent -Event 'qwen_down_completed' -Details @{ path = $path }
}

function Invoke-StartHub {
    $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if (-not $task) {
        Install-SupervisorTask
        $task = Get-ScheduledTask -TaskName $taskName -ErrorAction Stop
    }
    if ($task.State -eq 'Disabled') {
        Enable-ScheduledTask -TaskName $taskName | Out-Null
    }
    if ((Get-ScheduledTask -TaskName $taskName).State -ne 'Running') {
        Start-ScheduledTask -TaskName $taskName
    }
    Invoke-Ensure
    Write-SupervisorEvent -Event 'localhub_up_completed' -Details @{
        scheduled_watch_available = $true
        qwen_desired = [bool](Get-DesiredUp)
    }
}

function Invoke-DownAll {
    $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if ($task) {
        Disable-ScheduledTask -TaskName $taskName | Out-Null
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    }

    # Clear durable model intent, then stop every LocalHub-managed instance before terminating the
    # control plane. Checking only the canonical Qwen port here missed instances started through the
    # normal /api/instances path (web UI, startup config, reconcile) on a different port: DownAll
    # would skip straight to killing the daemon and leave that engine process running, still holding
    # VRAM. Invoke-Down's up-to-90s listener-port poll is too slow for the tray exit deadline, so use
    # a bounded force=true delete per instance instead (LocalHub's own Stop() is graceful-then-Job-
    # Object-kill within ~5-7s per internal/instances/proc/proc_windows.go GraceTimeout).
    Set-DesiredDown -Reason 'operator invoked DownAll'
    # StopInstanceAsyncForce (internal/core/instances.go) returns as soon as the stop is ACCEPTED —
    # the real kill (graceful -> Job Object hard-kill, up to proc.GraceTimeout=5s, + reap) runs in a
    # background goroutine OWNED BY THE DAEMON PROCESS. If we force-kill the daemon right after a 200
    # response, that goroutine dies mid-flight and the engine child survives orphaned (observed: the
    # first version of this fix did exactly that). So poll each instance to a confirmed terminal state
    # before moving on. Also re-scan afterward: stopping one instance can free enough VRAM for
    # LocalHub's own queue-drain loop to auto-admit a different queued instance seconds later
    # (observed in logs/qwen-localhub-supervisor.jsonl) — a single pass over the instance list can
    # miss it. Bounded to 45s total so a stuck daemon still can't hang the tray's exit indefinitely.
    $shutdownDeadline = (Get-Date).AddSeconds(45)
    do {
        $active = Get-LocalHubInstances | Where-Object { $_.state -in @('HEALTHY', 'STARTING', 'DRAINING') }
        foreach ($instance in $active) {
            try {
                Invoke-RestMethod -Method Delete `
                    -Uri "$localHubBaseURL/api/instances/$($instance.id)?force=true" `
                    -TimeoutSec 10 | Out-Null
            }
            catch {
                Write-SupervisorEvent -Event 'instance_down_failed' -Level 'warning' -Details @{
                    instance_id = $instance.id
                    error = $_.Exception.Message
                }
                continue
            }
            $instanceDeadline = (Get-Date).AddSeconds(15)
            do {
                Start-Sleep -Milliseconds 500
                $current = Get-LocalHubInstances | Where-Object { $_.id -eq $instance.id }
            } while ($current -and $current.state -in @('HEALTHY', 'STARTING', 'DRAINING') -and (Get-Date) -lt $instanceDeadline)
            if ($current -and $current.state -in @('HEALTHY', 'STARTING', 'DRAINING')) {
                Write-SupervisorEvent -Event 'instance_down_timed_out' -Level 'warning' -Details @{ instance_id = $instance.id }
            }
            else {
                Write-SupervisorEvent -Event 'instance_down_completed' -Details @{ instance_id = $instance.id }
            }
        }
    } while ((Get-LocalHubInstances | Where-Object { $_.state -in @('HEALTHY', 'STARTING', 'DRAINING') }) -and (Get-Date) -lt $shutdownDeadline)
    if (Get-Listener -Port $qwenPort) {
        & $qwenLauncher Stop
        if (Get-Listener -Port $qwenPort) {
            throw "Full shutdown incomplete: Qwen is still listening on port $qwenPort after launcher stop."
        }
        Write-SupervisorEvent -Event 'qwen_down_completed' -Details @{ path = 'down_all_launcher' }
    }

    $process = Get-ListenerProcess -Port $localHubPort
    if ($process) {
        if (-not (Test-ExpectedLocalHubProcess -Process $process)) {
            throw "Refusing full shutdown: port $localHubPort belongs to unexpected process PID $($process.ProcessId)."
        }
        Stop-Process -Id $process.ProcessId -Force
        Wait-Process -Id $process.ProcessId -Timeout 30 -ErrorAction SilentlyContinue
    }

    if (Get-Listener -Port $qwenPort) {
        throw "Full shutdown incomplete: Qwen is still listening on port $qwenPort."
    }
    if (Get-Listener -Port $localHubPort) {
        throw "Full shutdown incomplete: LocalHub is still listening on port $localHubPort."
    }
    Write-SupervisorEvent -Event 'localhub_down_all_completed' -Details @{
        scheduled_watch_disabled = [bool]$task
    }
}

function Show-SupervisorStatus {
    $listener = Get-Listener -Port $qwenPort
    $process = Get-ListenerProcess -Port $qwenPort
    $tracked = Get-QwenInstances | Where-Object { $_.state -eq 'HEALTHY' } | Select-Object -First 1
    $task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    [pscustomobject]@{
        ScheduledTask = if ($task) { $task.State } else { 'Not installed' }
        LocalHub = if (Test-LocalHubHealth) { 'healthy' } else { 'down/unhealthy' }
        LocalHubURL = $localHubBaseURL
        QwenDesired = if (Get-DesiredUp) { 'up (auto-recover)' } else { 'down (stay down)' }
        QwenHealth = if (Test-QwenHealth) { 'healthy' } else { 'down/unhealthy' }
        QwenPID = if ($listener) { $listener.OwningProcess } else { $null }
        ExpectedProfile = [bool](Test-ExpectedQwenProcess -Process $process)
        LocalHubInstance = if ($tracked) { $tracked.id } else { $null }
        LocalHubInstanceState = if ($tracked) { $tracked.state } else { 'not attached/healthy' }
    } | Format-List
}

function Install-SupervisorTask {
    $taskAction = New-ScheduledTaskAction `
        -Execute 'powershell.exe' `
        -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$PSCommandPath`" Watch -EnsureIntervalMinutes $EnsureIntervalMinutes"
    $logonTrigger = New-ScheduledTaskTrigger -AtLogOn -User ([Environment]::UserName)
    # MultipleInstances=IgnoreNew means this trigger does not launch another PowerShell while
    # Watch is alive. It only recreates Watch after an external force-kill, which Task
    # Scheduler does not classify as an action failure eligible for RestartOnFailure.
    $rescueTrigger = New-ScheduledTaskTrigger -Once `
        -At (Get-Date).AddMinutes($RescueIntervalMinutes) `
        -RepetitionInterval (New-TimeSpan -Minutes $RescueIntervalMinutes)
    $settings = New-ScheduledTaskSettingsSet `
        -StartWhenAvailable `
        -MultipleInstances IgnoreNew `
        -ExecutionTimeLimit ([timespan]::Zero) `
        -RestartCount 3 `
        -RestartInterval (New-TimeSpan -Minutes 1)
    $description = 'Event-driven LocalHub and Qwen3.6 supervisor; explicit Down stays down; periodic health fallback detects hangs.'
    $existingTask = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if ($existingTask) {
        Set-ScheduledTask `
            -TaskName $taskName `
            -Action $taskAction `
            -Trigger @($logonTrigger, $rescueTrigger) `
            -Settings $settings | Out-Null
    }
    else {
        Register-ScheduledTask `
            -TaskName $taskName `
            -Action $taskAction `
            -Trigger @($logonTrigger, $rescueTrigger) `
            -Settings $settings `
            -Description $description | Out-Null
    }
    Write-SupervisorEvent -Event 'scheduled_task_installed' -Details @{
        task_name = $taskName
        mode = 'event_driven_watch'
        health_fallback_minutes = $EnsureIntervalMinutes
        watch_rescue_minutes = $RescueIntervalMinutes
    }
}

$mutex = $null
$hasMutex = $false
try {
    if ($Action -ne 'Status' -and $Action -ne 'Watch') {
        $mutex = New-Object System.Threading.Mutex($false, 'LocalHubQwenSupervisor')
        try {
            $mutexWaitMilliseconds = if ($Action -in @('StartHub', 'Up', 'Down', 'DownAll', 'Install', 'Uninstall')) {
                30000
            }
            else {
                0
            }
            $hasMutex = $mutex.WaitOne($mutexWaitMilliseconds)
        }
        catch [System.Threading.AbandonedMutexException] {
            $hasMutex = $true
        }
        if (-not $hasMutex) {
            Write-SupervisorEvent -Event 'action_skipped_concurrent_run' -Level 'warning'
            if ($Action -in @('StartHub', 'Up', 'Down', 'DownAll', 'Install', 'Uninstall')) {
                throw "Another LocalHub supervisor action is still running after 30 seconds."
            }
            return
        }
    }

    switch ($Action) {
        'Ensure' {
            Invoke-Ensure
        }
        'Watch' {
            Invoke-Watch
        }
        'StartHub' {
            Invoke-StartHub
            Show-SupervisorStatus
        }
        'StartHubOnce' {
            Start-LocalHub
            Show-SupervisorStatus
        }
        'Up' {
            Set-DesiredUp
            Invoke-Ensure
            Show-SupervisorStatus
        }
        'Down' {
            Invoke-Down
            Show-SupervisorStatus
        }
        'DownAll' {
            Invoke-DownAll
            Show-SupervisorStatus
        }
        'Status' {
            Show-SupervisorStatus
        }
        'Install' {
            Install-SupervisorTask
            Set-DesiredUp
            Invoke-Ensure
            if ((Get-ScheduledTask -TaskName $taskName).State -ne 'Running') {
                Start-ScheduledTask -TaskName $taskName
            }
            Show-SupervisorStatus
        }
        'Uninstall' {
            Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
            Write-SupervisorEvent -Event 'scheduled_task_uninstalled' -Details @{ task_name = $taskName }
            Show-SupervisorStatus
        }
    }
}
finally {
    if ($hasMutex -and $mutex) {
        $mutex.ReleaseMutex()
    }
    if ($mutex) {
        $mutex.Dispose()
    }
}
