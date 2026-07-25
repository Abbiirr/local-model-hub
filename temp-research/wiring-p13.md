# Wiring notes — P1.3 engine detection (read-only)

Author: P1.3 PACKAGE agent. Scope of this file: how the `internal/engines/` detection
adapters wire into the rest of the daemon. The wiring itself (edits to `internal/core`,
`internal/api`, `internal/config`) is OWNED BY OTHER AGENTS — this file records the exact
seams so they can be added without re-deriving them. This agent touched only
`internal/engines/` and this file.

Verified against the reference box 2026-07-06. All calls in this package are strictly
read-only (GET-only HTTP; `--version`/`--help`/`docker --version`/`docker images` only). No
POST/PUT/DELETE, no `docker run`/`docker pull`, no daemon start. Mutation is Phase 4.

## Packages delivered

- `internal/engines/types.go` — `EngineKind` (ollama|llamacpp|vllm), `EngineStatus`,
  `Detector` interface.
- `internal/engines/ollama` — `Client` with `New(baseURL, binaryPath)`; `Detect(ctx)`,
  `Version(ctx)`, `ListModels(ctx)` (GET /api/tags), `ListRunning(ctx)` (GET /api/ps).
- `internal/engines/llamacpp` — `Adapter`; `New([]BinarySpec)` and
  `NewFromPaths(paths, extraPath)`; `Detect(ctx) []EngineStatus` (one per binary).
- `internal/engines/vllm` — `Adapter`; `New(mode, endpoint)`; `Detect(ctx) EngineStatus`
  (attach: GET /health + /version; docker: `docker --version` + `docker images` filter).

## Deps struct (internal/core.Deps) — proposed additive fields

Add to `core.Deps` (P1.2 added a Hardware seam the same way). Suggested shape:

```go
// In internal/core/core.go, Deps struct:
Engines EnginesRegistry   // read-only engine detectors; nil-safe (Engines() reports empty)
```

Where `EnginesRegistry` is a small struct the wiring agent defines in core (or an
engines-side constructor if preferred), holding the three constructed adapters:

```go
type EnginesRegistry struct {
    Ollama   *ollama.Client     // or nil if engines.ollama.enabled=false
    LlamaCPP *llamacpp.Adapter  // or nil if engines.llamacpp.enabled=false
    VLLM     *vllm.Adapter      // or nil if engines.vllm.enabled=false
}
```

Construction (in `cmd/localhub` main, from `config.Config`):

```go
reg := core.EnginesRegistry{}
if cfg.Engines.Ollama.Enabled {
    reg.Ollama = ollama.New(cfg.Engines.Ollama.BaseURL, "") // "" => DefaultBinaryPath
}
if cfg.Engines.LlamaCPP.Enabled {
    reg.LlamaCPP = llamacpp.NewFromPaths(cfg.Engines.LlamaCPP.BinaryPaths, llamaExtraPath)
}
if cfg.Engines.VLLM.Enabled {
    ep := cfg.Engines.VLLM.AttachURL // attach mode uses this; docker mode ignores it
    reg.VLLM = vllm.New(cfg.Engines.VLLM.Mode, ep)
}
```

## Core method — `Engines(ctx)`

Add one method to `internal/core.Service` following the `Health(ctx)` pattern (context in,
typed result out, no transport concerns):

```go
// EngineStatusView is the API projection of engines.EngineStatus (see mapping below).
func (s *Service) Engines(ctx context.Context) []EngineStatusView
```

Implementation: iterate the registry, call each adapter's `Detect`, flatten (llamacpp
returns a slice), map each `engines.EngineStatus` to the API shape. Wrap each Detect in a
short per-engine timeout (derive a child context, e.g. 3s Ollama/HTTP, 10s llamacpp/binary
probes, 8s vLLM/docker) so one slow engine never stalls the sweep. A nil adapter (engine
disabled in config) yields a single `Detected=false` entry with a Reason like
`"engine <kind> disabled in config"`.

## Route — `GET /api/engines`

api-spec.md §3 shape: `[{name, installed, version, running, base_url}]`. The internal
`EngineStatus` maps to it as:

| API field   | EngineStatus field | note |
|-------------|--------------------|------|
| `name`      | `Kind`             | ollama\|llamacpp\|vllm |
| `installed` | `Detected`         | binary/daemon/config present |
| `version`   | `Version`          | "" when unknown |
| `running`   | `Reachable`        | daemon/endpoint answering now |
| `base_url`  | `Endpoint`         | "" for llamacpp (spawned, no daemon) |
| `reason`    | `Reason`           | ALWAYS present (non-waivable); add to the response even though the bare api-spec tuple omits it — every verdict carries a reason |
| `details`   | `Details`          | recommend surfacing (backends, caps, running_models, images) for the dashboard |

Note: llama.cpp produces MULTIPLE entries (one per binary). The `name` is `llamacpp` for
each; disambiguate in the UI via `details.binary_path` / `details.resolved_path`. If the
API must key uniquely, the wiring agent may append the binary identity — but the DB
`engines` table PK is `name` (single row per engine); the per-binary detail belongs in the
API/`details` view, not necessarily one `engines` row each. Flag for the DB-mapping agent:
decide whether `engines` stores the primary llama-server binary only (recommended: yes,
store the primary/best binary in `engines.binary_path`; keep the full multi-binary list in
the live API response).

`POST /api/engines/detect` (re-probe now) simply re-calls `Service.Engines(ctx)`; it is
read-only despite being a POST (no engine mutation) — safe under the P1.3 zero-mutation
gate.

## Config mapping (internal/config — already present, no changes needed)

The existing `config.EnginesConfig` already carries everything the adapters need:

- `Engines.Ollama.Enabled`, `Engines.Ollama.BaseURL` — Ollama endpoint. Binary path is not
  in config; the adapter defaults to `ollama.DefaultBinaryPath`
  (`K:\Softwares\ollama\ollama.exe`). If a configurable Ollama binary path is ever wanted,
  add `Engines.Ollama.BinaryPath` and pass it as the 2nd arg to `ollama.New`.
- `Engines.LlamaCPP.Enabled`, `Engines.LlamaCPP.BinaryPaths` (`[]string`, already folds the
  legacy single `BinaryPath` in `normalize()`), `Engines.LlamaCPP.PortRange`.
- `Engines.VLLM.Enabled`, `Engines.VLLM.Mode` (docker|attach|subprocess),
  `Engines.VLLM.AttachURL`. **Naming note:** the task brief called the vLLM field
  `Endpoint`; the actual config field is `AttachURL` (used only in `attach` mode). No config
  change needed — the adapter's `New(mode, endpoint)` takes `AttachURL` for `endpoint`.

### ExtraPath need (llama.cpp CUDA fork) — CONFIG GAP to fill

The CUDA fork at `K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe` needs
`K:\Softwares\Nvidia GPU Compiling Toolkit\CUDA\v12.5\bin` prepended to PATH so its CUDA
DLLs load at probe/spawn time. The adapter supports this via `BinarySpec.ExtraPath` (and
`NewFromPaths(paths, extraPath)`), but **`config.LlamaCPPConfig` has no ExtraPath field
yet**. Recommended additive config change (owned by the config agent):

```go
type LlamaCPPConfig struct {
    // ...existing...
    ExtraPath []string `yaml:"extra_path"` // dirs prepended to PATH for explicit-path binaries (CUDA fork DLLs)
}
```

Until that lands, the wiring in `cmd/localhub` can hardcode the reference-box value
(`llamaExtraPath := []string{`K:\Softwares\Nvidia GPU Compiling Toolkit\CUDA\v12.5\bin`}`)
or pass `nil` (the winget build works without it; the fork's `--version`/`--help` probe may
fail to load CUDA DLLs and report the binary as not-detected). `NewFromPaths` only attaches
ExtraPath to EXPLICIT paths (absolute), never to a bare PATH name like `llama-server`, so
the winget build is unaffected.

## Verified-current Ollama field shapes (source + date)

Source: https://github.com/ollama/ollama/blob/main/docs/api.md — fetched 2026-07-06.
Cross-checked live against the reference-box daemon (Ollama 0.21.2, `/api/version` returned
`{"version":"0.21.2"}`). Parsed defensively (unknown fields ignored, missing fields zero).

- `GET /api/version` -> `{"version": string}`.
- `GET /api/tags` -> `{"models": [ {name, model, modified_at, size(int), digest,
  details:{parent_model, format, family, families:[string], parameter_size,
  quantization_level}} ]}`.
- `GET /api/ps` -> `{"models": [ {name, model, size(int), digest, details:{...same...},
  expires_at(string), size_vram(int)} ]}`. `size_vram` (bytes resident in VRAM) and
  `expires_at` (idle-unload time) are the residency source-of-truth fields
  (engine-adapters.md §5). These are exposed as `PSModel.SizeVRAM` / `PSModel.ExpiresAt`
  for the P4.3 residency work.

## Real-box detection results (2026-07-06)

Run of all three `Detect` against reality (harness deleted after run):

- **Ollama**: `detected=true, reachable=true, version=0.21.2`, 5 installed models, 0
  running. (Task anticipated the daemon might be stopped; on this run it was UP.)
- **llama.cpp** (2 statuses):
  - winget `llama-server` (PATH): `detected=true`, version `8833 (45cac7ca7)`, backends
    `[RPC, Vulkan, CPU]`, caps `{fit, sleep-idle, flash-attn}`, **has_tq3=false**.
  - CUDA fork (explicit path + ExtraPath): `detected=true`, version `1 (1cfa891)`, backends
    `[CUDA]`, caps `{fit, sleep-idle, flash-attn, tq3}`, **has_tq3=true**. ExtraPath
    recorded in `details.extra_path`.
- **vLLM** (docker mode): `detected=true, reachable=false`. **FINDING — differs from task
  assumption:** the box ALREADY has vLLM images locally (`vllm/vllm-omni:latest`,
  `vllm/vllm-openai:latest`), so the correct verdict is "image present, not started"
  (reason references P4.5 launch), not "no image present". The adapter's image-present
  branch handled this. The "no vllm image present; will be provisioned in P4.5" reason
  (task-specified) still fires when no image exists — verified by unit test
  `TestDetectDocker_NoImage`.

## Deps recorded (per coordination rule)

None. Implemented with stdlib only (`net/http`, `encoding/json`, `os/exec`, `regexp`,
`bytes`). No `go.mod` change required.
