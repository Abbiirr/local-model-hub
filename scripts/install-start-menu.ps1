<#
.SYNOPSIS
  Create (or remove) the Windows Start Menu entry for LocalHub.

.DESCRIPTION
  Creates Start Menu > Programs > LocalHub > "LocalHub", pointing at the tray app, so LocalHub
  can be launched from the Start Menu at any time (Start > type "LocalHub"). This is separate
  from "Start with Windows" (the HKCU Run key toggled from the tray's own "Startup behavior"
  menu / internal/trayapp's StartupEnabled/SetStartupEnabled) - this script only adds a way to
  launch on demand.

  ONE shortcut, not two. Earlier versions also created a "LocalHub Daemon" shortcut, which is
  now both redundant and misleading: the tray supervises the daemon itself (it health-checks
  http://127.0.0.1:39876/api/health and starts bin/localhub.exe only if nothing answers), so
  launching the daemon separately just creates a control plane the tray did not start. Any
  leftover "LocalHub Tray.lnk" / "LocalHub Daemon.lnk" from those versions is removed here.

  The shortcut's icon comes from assets/localhub.ico - the same file the tray embeds - so the
  Start Menu entry and the notification-area icon are the same mark instead of the generic
  executable glyph Windows falls back to for an .exe with no icon resource.

  Idempotent: re-running overwrites the same .lnk in place rather than creating duplicates.
  Safe to run repeatedly, e.g. after rebuilding the binaries.

.PARAMETER RepoRoot
  Repository root used to resolve the tray executable and as the shortcut's working directory.
  Defaults to the parent of this script's directory, so the script works unmodified on any
  machine that clones the repo to a different path.

.PARAMETER Uninstall
  Remove the "LocalHub" Start Menu folder (and the Startup shortcut, if present) instead of
  creating the shortcut.

.PARAMETER Startup
  Also place a copy of the shortcut in the per-user Startup folder
  ([Environment]::GetFolderPath('Startup')), so the tray auto-launches at login. Off by default.

.EXAMPLE
  ./scripts/install-start-menu.ps1
  Creates Start Menu > LocalHub > "LocalHub".

.EXAMPLE
  ./scripts/install-start-menu.ps1 -Startup
  Same as above, and also auto-launches the tray at Windows login.

.EXAMPLE
  ./scripts/install-start-menu.ps1 -Uninstall
  Removes the Start Menu folder and Startup shortcut created by this script.
#>
param(
    [string]$RepoRoot = (Split-Path -Parent $PSScriptRoot),
    [switch]$Uninstall,
    [switch]$Startup
)

$ErrorActionPreference = 'Stop'

$RepoRoot = (Resolve-Path -LiteralPath $RepoRoot).Path
$trayExe = Join-Path $RepoRoot 'localhub-tray.exe'
$iconFile = Join-Path $RepoRoot 'assets\localhub.ico'

$startMenuPrograms = [Environment]::GetFolderPath('Programs')
$startupFolder = [Environment]::GetFolderPath('Startup')
$localHubFolder = Join-Path $startMenuPrograms 'LocalHub'
$shortcutPath = Join-Path $localHubFolder 'LocalHub.lnk'
$startupShortcutPath = Join-Path $startupFolder 'LocalHub.lnk'

# Shortcuts created by earlier versions of this script. Removed on both install and uninstall so
# the Start Menu never shows three entries for one app.
$legacyShortcuts = @(
    (Join-Path $localHubFolder 'LocalHub Tray.lnk'),
    (Join-Path $localHubFolder 'LocalHub Daemon.lnk'),
    (Join-Path $startupFolder 'LocalHub Tray.lnk')
)

# Explorer caches the Start Menu's contents. A freshly written .lnk can therefore be present on
# disk yet absent from Start until the shell is told the folder changed - which is the usual
# reason "I ran the script but LocalHub does not appear". SHCNE_UPDATEDIR asks Explorer to
# re-read the folder; SHCNE_ASSOCCHANGED nudges the icon cache for the new IconLocation.
function Update-ShellFolder {
    param([Parameter(Mandatory)][string]$Path)
    if (-not ('LocalHub.ShellNotify' -as [type])) {
        Add-Type -Namespace 'LocalHub' -Name 'ShellNotify' -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("shell32.dll", CharSet = System.Runtime.InteropServices.CharSet.Unicode)]
public static extern void SHChangeNotify(int eventId, uint flags, string item1, string item2);
'@
    }
    $SHCNE_UPDATEDIR = 0x00001000
    $SHCNE_ASSOCCHANGED = 0x08000000
    $SHCNF_PATHW = 0x0005
    $SHCNF_FLUSH = 0x1000
    [LocalHub.ShellNotify]::SHChangeNotify($SHCNE_UPDATEDIR, $SHCNF_PATHW -bor $SHCNF_FLUSH, $Path, $null)
    [LocalHub.ShellNotify]::SHChangeNotify($SHCNE_ASSOCCHANGED, $SHCNF_FLUSH, $null, $null)
}

function New-AppShortcut {
    param(
        [Parameter(Mandatory)][string]$ShortcutPath,
        [Parameter(Mandatory)][string]$TargetPath,
        [Parameter(Mandatory)][string]$Description,
        [string]$Arguments = '',
        [string]$IconPath = ''
    )
    $shell = New-Object -ComObject WScript.Shell
    try {
        $shortcut = $shell.CreateShortcut($ShortcutPath)
        $shortcut.TargetPath = $TargetPath
        if ($Arguments) { $shortcut.Arguments = $Arguments }
        $shortcut.WorkingDirectory = $RepoRoot
        $shortcut.Description = $Description
        if ($IconPath -and (Test-Path -LiteralPath $IconPath)) {
            $shortcut.IconLocation = "$IconPath,0"
        }
        else {
            # No icon asset: fall back to the executable's own (generic) icon rather than
            # writing a broken IconLocation that renders as a blank page.
            $shortcut.IconLocation = "$TargetPath,0"
        }
        $shortcut.Save()
    }
    finally {
        [void][System.Runtime.InteropServices.Marshal]::ReleaseComObject($shell)
    }
}

foreach ($legacy in $legacyShortcuts) {
    if (Test-Path -LiteralPath $legacy) {
        Remove-Item -LiteralPath $legacy -Force
        Write-Host "Removed legacy shortcut $legacy" -ForegroundColor Yellow
    }
}

if ($Uninstall) {
    if (Test-Path -LiteralPath $localHubFolder) {
        Remove-Item -LiteralPath $localHubFolder -Recurse -Force
        Write-Host "Removed $localHubFolder" -ForegroundColor Yellow
    }
    else {
        Write-Host "Nothing to remove: $localHubFolder does not exist" -ForegroundColor Yellow
    }
    if (Test-Path -LiteralPath $startupShortcutPath) {
        Remove-Item -LiteralPath $startupShortcutPath -Force
        Write-Host "Removed $startupShortcutPath" -ForegroundColor Yellow
    }
    Update-ShellFolder -Path $startMenuPrograms
    return
}

if (-not (Test-Path -LiteralPath $trayExe)) {
    throw "Tray executable not found at $trayExe - build it first: ./scripts/build-localhub-tray.ps1"
}
if (-not (Test-Path -LiteralPath $iconFile)) {
    Write-Warning "Icon not found at $iconFile - the shortcut will use the generic executable icon."
}

New-Item -ItemType Directory -Force -Path $localHubFolder | Out-Null

New-AppShortcut -ShortcutPath $shortcutPath -TargetPath $trayExe `
    -Description 'LocalHub Compass - control plane status, start and stop models' `
    -Arguments "-repo `"$RepoRoot`"" -IconPath $iconFile
Write-Host "Created $shortcutPath" -ForegroundColor Green

if ($Startup) {
    New-AppShortcut -ShortcutPath $startupShortcutPath -TargetPath $trayExe `
        -Description 'LocalHub Compass (auto-launch at login)' `
        -Arguments "-repo `"$RepoRoot`"" -IconPath $iconFile
    Write-Host "Created $startupShortcutPath (auto-launch at login enabled)" -ForegroundColor Green
    Update-ShellFolder -Path $startupFolder
}

Update-ShellFolder -Path $localHubFolder
Update-ShellFolder -Path $startMenuPrograms

Write-Host ''
Write-Host 'Start Menu entry installed. Press Start and type "LocalHub".' -ForegroundColor Cyan
Write-Host "  Target : $trayExe"
Write-Host "  Icon   : $iconFile"
Write-Host "  Folder : $localHubFolder"
Write-Host 'If it still does not appear in Start search, Windows Search has not indexed it yet;'
Write-Host 'the folder itself is browsable immediately under Start > All apps > LocalHub.'
