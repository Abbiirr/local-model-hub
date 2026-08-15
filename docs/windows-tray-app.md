# LocalHub Windows tray app

Build and install the current-user tray application:

```powershell
scripts\localhub-tray.cmd Install
```

After changing source code, publish the complete current Windows distribution with:

```powershell
scripts\publish-windows-app.ps1
```

It stages both binaries before touching the live stack, drains models through the native Exit
controller, replaces the stable daemon/tray paths, refreshes the one Start Menu shortcut, relaunches,
and waits for every enabled startup model. A failed build leaves the current running distribution
untouched.

The tray app is a native Windows GUI executable registered through the current-user
Windows **Startup Apps** surface. It can be enabled or disabled from the tray menu or Task
Manager's **Startup apps** page. Installation removes the older `LocalHub-Tray` Scheduled
Task so there is only one startup authority.

The tray menu provides:

- **Open Web UI** — opens `http://127.0.0.1:39876` in the default browser. Never disabled.
- **Start LocalHub** — starts the control plane if nothing is answering. The tray
  health-checks `GET /api/health` first and adopts a daemon that is already running, so this
  item reads **LocalHub is running** (and is disabled) whenever the control plane is up.
- **Startup behavior** — controls Start with Windows, LocalHub startup, Web UI opening,
  previous-session restoration, and login delay.
- **Models at startup** — enables models and controls their saved launch profile, load
  delay, order, and retry policy. Every model uses LocalHub's admission-controlled
  `POST /api/instances` path; a refused start surfaces the server's free-versus-required
  VRAM reason.
- **Exit LocalHub** — cancels any startup/recovery action, stops running models through the
  normal drain-aware API, stops the daemon, and closes the tray. A daemon started by an earlier
  LocalHub process is stopped only after its health-reported PID resolves to `bin\localhub.exe`
  or `localhub.exe` in this checkout. Exit stays open and reports a reason if the complete stack
  cannot be verified down; it never silently leaves LocalHub processes behind.

There are no model-specific menu items. An earlier version hardcoded **Start Qwen3.6** /
**Stop Qwen3.6** for one artifact and drove them through
`scripts/keep-localhub-qwen.ps1`; both the items and that dependency are gone.

## No PowerShell, and no stuck menu

The tray supervises `bin\localhub.exe` (falling back to `localhub.exe` in the repo root)
directly with `os/exec`, spawned via `internal/winproc.Hide` so no console window appears,
then polls `/api/health` with backoff until healthy or a bounded timeout.

It previously ran `powershell.exe -File scripts\keep-localhub-qwen.ps1 StartHubOnce` and
read the result with `CombinedOutput`. The daemon inherited that pipe, so the read never
reached EOF and the call never returned — leaving the menu disabled behind a frozen
"Starting LocalHub..." status line. Two invariants now prevent that class of failure:

1. Every menu action runs through one helper that takes the busy lock, applies a deadline,
   and restores the menu from a `defer` — on success, failure, timeout, or panic.
2. Nothing the tray spawns is read through an inherited pipe.

`scripts/keep-localhub-qwen.ps1` and `scripts/run-qwen-64k-q4q4.ps1` remain in the repo as
standalone manual tools. The tray does not call them.

## Start Menu entry

```powershell
scripts\install-start-menu.ps1          # Start > All apps > LocalHub > LocalHub
scripts\install-start-menu.ps1 -Startup # also auto-launch at login
scripts\install-start-menu.ps1 -Uninstall
```

One shortcut, pointing at the tray. Its icon is `assets/localhub.ico`, the same file the
tray embeds with `go:embed`, so the Start Menu entry and the notification-area icon match
instead of falling back to a generic executable glyph. The script removes shortcuts left by
earlier versions (`LocalHub Tray.lnk`, `LocalHub Daemon.lnk`) and notifies the shell
(`SHChangeNotify`) so Explorer picks the new entry up without waiting for a cache refresh.

Command-line maintenance:

```powershell
scripts\localhub-tray.cmd Status
scripts\localhub-tray.cmd Start
scripts\localhub-tray.cmd Stop
scripts\localhub-tray.cmd Uninstall
```

`Stop` closes only the tray process. Use the tray's **Exit LocalHub** item or
`scripts\localhub-qwen.cmd DownAll` to stop the whole stack.

The complete policy is stored atomically in `configs/startup.json` (schema **v2**). The Web
UI's **Startup behavior** panel exposes engine/profile fields, context, KV cache types,
slots, speculative mode, priority, pinning, CPU-offload opt-in, per-model retry limits,
delays, and ordering. Startup never bypasses normal compatibility/admission checks, never
kills an in-flight request, and reports the server's human-readable refusal reason.

**v2 ships with no startup models**, and deleting `configs/startup.json` recreates it empty.
The v1 default seeded one enabled 27B model, so removing the file silently re-enabled a large
load at the next login. A v1 file is upgraded in place on first read: its `canonical_qwen`
entries are dropped (that mode's only mechanism was the PowerShell supervisor) and the
migrated document is written back, so the upgrade happens once. `localhub_api` is the only
remaining mode.

`keep_available` uses a configurable low-frequency health fallback (60 seconds by default),
not a once-per-minute Scheduled Task invocation. `never` performs one startup attempt;
`limited` retries across health cycles up to the configured attempt limit.

The executable is built as a Windows GUI application (`-ldflags "-H=windowsgui"`, PE
subsystem 2), so opening it does not create a console window.

Building requires **Go only** — no C compiler. `fyne.io/systray`'s Windows backend is pure Go
(it calls user32/shell32 through `golang.org/x/sys/windows`); only its Linux and macOS
backends need CGO. `scripts/build-localhub-tray.ps1` pins `CGO_ENABLED=0`.
