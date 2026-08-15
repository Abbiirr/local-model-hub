# Qwen3.6 availability through LocalHub

The workstation-level availability controller is:

```powershell
scripts\localhub-qwen.cmd Install
scripts\localhub-qwen.cmd Install -EnsureIntervalMinutes 10
```

`Install` creates the current-user scheduled task
`LocalHub-Qwen3.6-KeepAvailable`, records Qwen's desired state as `up`, starts LocalHub
when needed, keeps the proven 64K q4/q4 Qwen launcher healthy, and attaches the live
process to LocalHub. The current-user task starts one persistent watchdog at logon. Windows
process lifecycle events trigger immediate reconciliation when LocalHub or llama-server
starts or exits; a health fallback runs every five minutes by default to detect a process
that is alive but hung. Use `EnsureIntervalMinutes` to adjust only that health fallback:

`powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\keep-localhub-qwen.ps1 Install -EnsureIntervalMinutes 10`

Normal controls:

```powershell
scripts\localhub-qwen.cmd Status
scripts\localhub-qwen.cmd Up
scripts\localhub-qwen.cmd Down
```

- `Up` means Qwen should be available and unexpected process failure should recover.
- `Down` is a durable operator stop. It clears desired-up **before** stopping Qwen, so
  the scheduled task will not race the stop or bring the model back.
- A clean stop through LocalHub, or an explicit `run-qwen-64k-q4q4.ps1 Stop`, is also
  recognized as operator intent and changes the desired state to down.
- A crash, hard kill, `FAILED` instance, LocalHub process failure, or a user logon is
  recovery work rather than operator intent.
- The task has no repeating one-minute health check. A 15-minute rescue trigger is retained
  for the watchdog process itself. With `MultipleInstances=IgnoreNew`, it launches no second
  PowerShell while Watch is alive; if Watch is externally killed, it recreates it. Set
  `RescueIntervalMinutes` during `Install` to change that worst-case watchdog recovery time.

LocalHub remains supervised even while Qwen is intentionally down. This keeps the
dashboard/control plane available without consuming Qwen's VRAM.

The former separately launched/adopted profile is retired. LocalHub must launch the
engine through `POST /api/instances` and serve it through its `/v1` API;
`POST /api/instances/attach` now returns `410 attach_disabled`.

Runtime evidence:

- `logs/qwen-localhub-supervisor.jsonl`
- `logs/qwen-localhub.desired-up.json` (present only while desired up)
- `logs/qwen-localhub.supervisor-state.json`
- the existing `logs/qwen-runtime-events.jsonl`

To remove only the scheduled recovery task:

```powershell
scripts\localhub-qwen.cmd Uninstall
```

`Uninstall` does not stop LocalHub or Qwen. Use `Down` first when the model should also
stop.
