# Wiring notes — P1.2 hardware probe (`internal/hardware`)

Author: P1.2 PACKAGE agent. Audience: whoever wires `internal/core`, `internal/api`,
`internal/config`, `cmd/localhub`. The `internal/hardware` package is complete, vetted, and
tested (unit + real-box). This doc says exactly how to plug it in. **No wiring was performed
by this agent** (coordination rule: hardware/ only).

## 1. What the package exposes (public API)

Package path: `github.com/localhub/localhub/internal/hardware`

- `type Probe interface { Snapshot(ctx context.Context) (Snapshot, error) }` — the seam to
  inject into `core.Deps`.
- `func New(cfg hardware.Config) Probe` — default composite probe for this OS (nvidia-smi +
  Windows CPU/RAM/disk). This is what the daemon constructs.
- `type Config struct { DiskPaths []string; SmiOptions []NvidiaSmiOption }`
- `func NewNvidiaSmiProbe(opts ...NvidiaSmiOption) *NvidiaSmiProbe` + options
  `WithBinary(path)`, `WithTimeout(d)` — only needed if you want GPU-only or a custom smi
  path/timeout.
- `type FakeProbe struct{ Snap; SnapFunc; Err; Calls }` + `DefaultFakeSnapshot()` — exported
  test double for core/api/compat/residency tests. Satisfies `Probe`.
- Types: `Snapshot`, `GPU`, `CPU`, `RAM`, `Disk` (JSON tags already match spec-digest §3).
- `var ErrNvidiaSmiNotFound` — sentinel; the composite probe already maps it to "0 GPUs",
  so callers normally never see it.

Snapshot semantics (important): **a missing nvidia-smi is NOT an error** — `Snapshot`
returns zero GPUs and a valid CPU/RAM/disk snapshot. `Snapshot` only returns a non-nil error
if reading CPU/RAM/disk fails AND there is no cached prior good snapshot. The probe caches
the last good snapshot internally, but polling cadence is the CALLER's job.

## 2. `internal/config` additions

**None required.** `HardwareConfig.PollIntervalSeconds` already exists (default 1) and
`DataConfig.ModelDirs` already exists — reuse both. Optional niceties if desired (all
additive, all have working defaults so omit them if you prefer zero churn):

```go
type HardwareConfig struct {
    NVIDIA              bool `yaml:"nvidia"`
    ROCm                bool `yaml:"rocm"`
    PollIntervalSeconds int  `yaml:"poll_interval_seconds"`
    // OPTIONAL additions (not required — package has sane defaults):
    NvidiaSmiPath       string `yaml:"nvidia_smi_path"`        // default: "nvidia-smi" on PATH
    NvidiaSmiTimeoutMs  int    `yaml:"nvidia_smi_timeout_ms"`  // default: 2000
}
```

If you add these, pass them through as `hardware.WithBinary(cfg.Hardware.NvidiaSmiPath)` and
`hardware.WithTimeout(time.Duration(ms)*time.Millisecond)` in `SmiOptions`. Otherwise skip.

## 3. `internal/core` wiring

### 3a. `Deps` field (exact)

```go
type Deps struct {
    Cfg      config.Config
    DB       *db.DB
    Hardware hardware.Probe   // <-- ADD THIS
}
```

### 3b. Core method (suggested signature)

```go
// Hardware returns a point-in-time hardware snapshot for GET /api/hardware and each SSE
// frame of GET /api/hardware/live. Thin passthrough to the injected probe; the probe already
// guarantees GPU-probe failure is non-fatal (zero GPUs, not an error).
func (s *Service) Hardware(ctx context.Context) (hardware.Snapshot, error) {
    if s.deps.Hardware == nil {
        return hardware.Snapshot{}, fmt.Errorf("hardware probe not configured")
    }
    return s.deps.Hardware.Snapshot(ctx)
}

// PollInterval is a convenience for the SSE handler / metrics loop.
func (s *Service) HardwarePollInterval() time.Duration {
    n := s.deps.Cfg.Hardware.PollIntervalSeconds
    if n < 1 { n = 1 }
    return time.Duration(n) * time.Second
}
```

Return the `hardware.Snapshot` directly — its JSON tags already produce the spec §3 shape,
so the API handler just `writeJSON(w, 200, snap)`.

### 3c. Construction in `cmd/localhub/main.go`

```go
hwProbe := hardware.New(hardware.Config{
    DiskPaths: cfg.Data.ModelDirs, // report free space on the model volumes
    // SmiOptions: []hardware.NvidiaSmiOption{hardware.WithTimeout(2*time.Second)}, // optional
})
svc := core.New(core.Deps{Cfg: cfg, DB: database, Hardware: hwProbe})
```

`DiskPaths` empty is fine (defaults to cwd volume), but passing `cfg.Data.ModelDirs` gives the
dashboard the disk the models actually live on (`K:\`).

## 4. `internal/api` route handlers

Register in `Server.routes`:

```go
mux.HandleFunc("GET /api/hardware", s.handleHardware)
mux.HandleFunc("GET /api/hardware/live", s.handleHardwareLive)  // SSE
```

### 4a. `GET /api/hardware` (thin)

```go
func (s *Server) handleHardware(w http.ResponseWriter, r *http.Request) {
    snap, err := s.svc.Hardware(r.Context())
    if err != nil {
        writeError(w, http.StatusInternalServerError, "hardware_probe_failed", err.Error())
        return
    }
    writeJSON(w, http.StatusOK, snap)
}
```

### 4b. `GET /api/hardware/live` (SSE, ~1 Hz, cadence from config)

```go
func (s *Server) handleHardwareLive(w http.ResponseWriter, r *http.Request) {
    fl, ok := w.(http.Flusher)
    if !ok { writeError(w, 500, "sse_unsupported", "streaming unsupported"); return }
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")

    interval := s.svc.HardwarePollInterval() // = Hardware.PollIntervalSeconds
    t := time.NewTicker(interval)
    defer t.Stop()

    write := func() bool {
        snap, err := s.svc.Hardware(r.Context())
        if err != nil { return true } // skip a frame on transient error; don't tear down
        b, _ := json.Marshal(snap)
        fmt.Fprintf(w, "data: %s\n\n", b)
        fl.Flush()
        return true
    }
    write() // send one immediately so the UI paints without waiting a full interval
    for {
        select {
        case <-r.Context().Done():
            return
        case <-t.C:
            write()
        }
    }
}
```

Notes: the probe is cheap-but-not-free (a `nvidia-smi` exec per call, ~50-120ms on this box).
At 1 Hz that is fine. If multiple SSE clients connect you may later want a single shared
poller feeding a fan-out, but for P1.2 per-connection polling is acceptable and simplest.
The metrics loop (P1.x/§9 observability) can call `svc.Hardware` on the same cadence to
persist to the `metrics` table (gpu_index, vram_used/total, util, temp, power).

## 5. JSON response shape (matched to spec-digest §3)

Spec §3: `{cpu, ram, disk, gpus:[{index,name,vram_total,vram_free,driver,cuda,cc,temp,power,util}]}`.
The `hardware.Snapshot` marshals to (real values from this box, 2026-07-06):

```json
{
  "gpus": [
    {
      "index": 0,
      "name": "NVIDIA GeForce RTX 4060 Ti",
      "vram_total": 16380,
      "vram_free": 13287,
      "vram_used": 2823,
      "driver": "576.40",
      "cc": "8.9",
      "util": 0,
      "temp": 55,
      "power": 14.76
    }
  ],
  "cpu": { "model": "AMD Ryzen 5 2600 Six-Core Processor", "cores": 6, "threads": 12, "util": -1 },
  "ram": { "total": 16331, "free": 4145, "used": 12186 },
  "disk": [
    { "path": "K:\\", "total_gib": 1863.01, "free_gib": 24.93, "used_gib": 1838.08 }
  ],
  "captured_at": "2026-07-06T07:05:33Z"
}
```

Deviations from the bare spec §3 keys, and why:
- **VRAM values are MiB, not bytes.** nvidia-smi `--nounits` reports memory in MiB; the spec
  uses `vram_total_mb`/`vram_free_mb` elsewhere (§4 verdict), so MiB is consistent. Key names
  are `vram_total`/`vram_free` per §3.
- **`vram_used` added** (§3 lists only total/free) — cheap, useful for the dashboard bar.
- **`cuda` is best-effort / usually empty.** The per-GPU CSV query does not expose the CUDA
  runtime version (it is an nvidia-smi header field, not a `--query-gpu` field). Left as
  `omitempty`. If you need it, add a second one-shot `nvidia-smi -q` parse later — not worth
  it for P1.2.
- **`disk` is a LIST**, not a single object. A box keeps models on 1..N volumes; the UI can
  show each. Spec §3 wrote `disk` singular; keeping the key `disk` but making it an array is
  the minimal honest representation. If the UI strictly wants one object, take `disk[0]`.
- **`cpu.util` = -1 (sentinel for "not measured").** CPU utilization needs perf-counter
  sampling over an interval; deferred from v1 (spec §9 lists CPU util as a metric, but the
  mandatory hardware fields are model/cores/threads). -1 is the documented "unavailable"
  sentinel. GPU `util`/`temp` are likewise -1 if nvidia-smi reports `[N/A]`.
- **`captured_at` added** (RFC3339 UTC) — lets the dashboard show staleness; harmless extra.

## 6. Dependency additions

**None.** The package uses only stdlib + `golang.org/x/sys/windows` (already an indirect dep
in go.mod v0.22.0 — no `go get` was run). No new module was added. `go.mod` was NOT touched.

## 7. Files created (all under internal/hardware/)

- `hardware.go` — types + `Probe` interface + JSON tags.
- `nvidia_smi.go` — `NvidiaSmiProbe` + pure `parseNvidiaSmiCSV` + exec runner (2s timeout,
  CREATE_NO_WINDOW).
- `probe.go` — composite `probe` (GPU + system), `SystemProbe` interface, last-good cache,
  `New`.
- `sys_windows.go` — `//go:build windows`: RAM (GlobalMemoryStatusEx), disk
  (GetDiskFreeSpaceEx), CPU model (registry) + physical cores
  (GetLogicalProcessorInformationEx), `hideWindow`.
- `sys_other.go` — `//go:build !windows`: compiling stub (zero RAM/disk, NumCPU for cores),
  no-op `hideWindow`. Real Linux probing is a later task.
- `util.go` — `round2`.
- `fake.go` — exported `FakeProbe` + `DefaultFakeSnapshot`.
- Tests: `nvidia_smi_test.go` (parser: healthy/multi/9-col/10-col/garbage/N-A/mixed + fake
  runner + composite-degrades-to-zero-GPUs), `sys_windows_test.go` (RAM/disk/CPU plausible),
  `fake_test.go`, `realbox_test.go` (real nvidia-smi + CPU/RAM/disk, skipped under -short).

## 8. Verification performed

- `go vet ./internal/hardware/...` — clean.
- `go test ./internal/hardware/... -short` — ok (all unit tests, hermetic).
- `go test ./internal/hardware/... -run TestRealBoxSnapshot -v` — PASS; detected RTX 4060 Ti
  16380 MiB, free 13287, CC 8.9, driver 576.40; CPU Ryzen 5 2600 6c/12t; RAM 16331 MiB total.
- `go build ./...` — `internal/hardware` and every other package build EXCEPT
  `internal/inventory` (P1.4, another agent's in-progress `scanner.go` — a pre-existing
  compile error in `scanner.go:7`/`:171`, unrelated to and untouched by P1.2).

## 9. Residency / compat consumers (forward note, no action for P1.2)

- The residency "re-measure free VRAM after each eviction via NVML" rule maps to
  `probe.Snapshot(ctx)` then read `GPUs[i].VRAMFreeMiB` (spec §13 conflict 3: "NVML" ==
  this probe interface). `FakeProbe.SnapFunc` + `FakeProbe.Calls` let residency tests assert
  free VRAM is re-read once per eviction (see `fake_test.go` TestFakeProbe_SnapFunc_CallCount).
- The compat engine compares its estimate against `GPUs[0].VRAMFreeMiB` (CURRENT free, MiB),
  not total.
```
