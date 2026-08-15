<#
.SYNOPSIS
  Manage Muse Glimmer 30B exclusively through LocalHub's owned-instance API.

.DESCRIPTION
  Starts the Windows tray app when needed, then asks LocalHub to launch the
  inventoried Muse model through POST /api/instances. This script never starts
  llama-server directly and never adopts an external process.

  The owned profile uses 65,536 context, one slot, q4_0 K/V cache, full GPU
  placement, pinned residency, and draft-free ngram-cache speculation. DFlash
  needs a second draft-model path, which LocalHub's native launch schema does
  not currently expose; the separate DFlash GGUF is therefore not used here.
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('Start', 'Status', 'Stop')]
    [string]$Action = 'Start',

    [string]$LocalHubURL = 'http://127.0.0.1:39876',

    [string]$ModelID = 'gguf-muse-glimmer-30b-ud-q3-k-xl'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$trayPath = Join-Path $repoRoot 'localhub-tray.exe'
$LocalHubURL = $LocalHubURL.TrimEnd('/')

function Get-LocalHubHealth {
    try {
        Invoke-RestMethod -Uri "$LocalHubURL/api/health" -TimeoutSec 3
    }
    catch { return $null }
}

function Ensure-LocalHubTray {
    $tray = Get-Process -Name 'localhub-tray' -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $tray) {
        if (-not (Test-Path -LiteralPath $trayPath -PathType Leaf)) {
            throw "LocalHub tray executable not found: $trayPath"
        }
        $tray = Start-Process -FilePath $trayPath `
            -ArgumentList @('-repo', $repoRoot) `
            -WorkingDirectory $repoRoot `
            -WindowStyle Normal `
            -PassThru
    }

    $deadline = (Get-Date).AddSeconds(60)
    do {
        $health = Get-LocalHubHealth
        if ($health) { return $tray }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)
    throw "The LocalHub tray is running as PID $($tray.Id), but $LocalHubURL/api/health did not become ready."
}

function Get-MuseInstances {
    (Invoke-RestMethod -Uri "$LocalHubURL/api/instances" -TimeoutSec 10) |
        Where-Object { $_.model_id -eq $ModelID }
}

function Wait-ForOwnedMuse {
    param([int]$TimeoutSeconds = 300)

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        $instance = Get-MuseInstances |
            Where-Object { $_.state -in 'STARTING', 'HEALTHY' } |
            Select-Object -First 1
        if ($instance -and $instance.adopted) {
            throw "Refusing adopted Muse instance $($instance.id): only LocalHub-owned starts are allowed."
        }
        if ($instance.state -eq 'HEALTHY') { return $instance }
        Start-Sleep -Seconds 2
    } while ((Get-Date) -lt $deadline)
    throw "Muse did not reach HEALTHY through LocalHub within $TimeoutSeconds seconds."
}

function Show-Status {
    $health = Get-LocalHubHealth
    $tray = Get-Process -Name 'localhub-tray' -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $health) {
        [pscustomobject]@{
            Tray             = if ($tray) { "running (PID $($tray.Id))" } else { 'stopped' }
            LocalHub         = 'unavailable'
            Model            = $ModelID
            Instance         = 'not running'
            LifecycleOwner   = 'none'
            ClientEndpoint   = "$LocalHubURL/v1"
        } | Format-List
        return
    }

    $instance = Get-MuseInstances |
        Where-Object { $_.state -in 'STARTING', 'HEALTHY', 'DRAINING' } |
        Select-Object -First 1
    [pscustomobject]@{
        Tray             = if ($tray) { "running (PID $($tray.Id))" } else { 'stopped' }
        LocalHub         = "healthy (PID $($health.pid))"
        Model            = $ModelID
        Instance         = if ($instance) { "$($instance.state) (PID $($instance.pid), port $($instance.port))" } else { 'not running' }
        Context          = if ($instance) { $instance.ctx } else { $null }
        Pinned           = if ($instance) { $instance.pinned } else { $null }
        Adopted          = if ($instance) { [bool]$instance.adopted } else { $null }
        LifecycleOwner   = if ($instance -and -not $instance.adopted) { 'LocalHub' } else { 'none' }
        ClientEndpoint   = "$LocalHubURL/v1"
    } | Format-List
    nvidia-smi --query-gpu=memory.total,memory.used,memory.free --format=csv,noheader
}

function Start-Muse {
    Ensure-LocalHubTray | Out-Null
    $existing = Get-MuseInstances |
        Where-Object { $_.state -in 'STARTING', 'HEALTHY' } |
        Select-Object -First 1
    if ($existing -and $existing.adopted) {
        throw "Refusing adopted Muse instance $($existing.id): the external attach path is sealed."
    }
    if ($existing.state -eq 'HEALTHY') {
        Write-Host "Already LocalHub-owned and healthy: PID $($existing.pid), port $($existing.port)." -ForegroundColor Green
        return $existing
    }

    if (-not $existing) {
        $body = @{
            model_id         = $ModelID
            engine           = 'llamacpp'
            ctx              = 65536
            kv_type_k        = 'q4_0'
            kv_type_v        = 'q4_0'
            slots            = 1
            speculative      = 'ngram-cache'
            priority         = 2
            pin              = $true
            allow_cpu_offload = $false
        } | ConvertTo-Json
        $response = Invoke-WebRequest -Method Post `
            -Uri "$LocalHubURL/api/instances" `
            -ContentType 'application/json' `
            -Body $body `
            -TimeoutSec 240
        if ($response.StatusCode -notin 201, 202) {
            throw "LocalHub returned unexpected start status $($response.StatusCode): $($response.Content)"
        }
    }

    $instance = Wait-ForOwnedMuse
    Write-Host "LocalHub-owned Muse is healthy: PID $($instance.pid), port $($instance.port), ctx $($instance.ctx)." -ForegroundColor Green
    $instance
}

function Stop-Muse {
    if (-not (Get-LocalHubHealth)) {
        throw 'LocalHub is unavailable; refusing to bypass its lifecycle manager.'
    }
    $instance = Get-MuseInstances |
        Where-Object { $_.state -in 'STARTING', 'HEALTHY', 'DRAINING' } |
        Select-Object -First 1
    if (-not $instance) {
        Write-Host 'Muse is already stopped.'
        return
    }
    if ($instance.adopted) {
        throw "Refusing adopted instance $($instance.id); external lifecycle management is sealed."
    }
    if ($instance.state -ne 'DRAINING') {
        Invoke-RestMethod -Method Delete `
            -Uri "$LocalHubURL/api/instances/$($instance.id)" `
            -TimeoutSec 15 | Out-Null
    }
    $deadline = (Get-Date).AddSeconds(120)
    do {
        Start-Sleep -Seconds 2
        $current = Get-MuseInstances | Where-Object { $_.id -eq $instance.id }
    } while ($current.state -eq 'DRAINING' -and (Get-Date) -lt $deadline)
    if ($current.state -ne 'STOPPED') {
        throw "Muse stop did not reach STOPPED: state=$($current.state), reason=$($current.reason)"
    }
    Write-Host "Stopped through LocalHub: $($current.reason)" -ForegroundColor Green
}

switch ($Action) {
    'Start'  { Start-Muse | Out-Null; Show-Status }
    'Status' { Show-Status }
    'Stop'   { Stop-Muse; Show-Status }
}
