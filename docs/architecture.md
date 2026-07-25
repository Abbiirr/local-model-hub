# Architecture

## Design constraints (in priority order)

1. **Single box first.** One host, 1–2 GPUs. No cluster assumptions leak into the data
   model or API. Multi-node is a *later* additive concern, never an MVP shape.
2. **The control plane never touches CUDA.** It shells out to engines. This is what lets
   it be a single static binary with a trivial install — the CUDA/driver pain stays
   inside the engines where it already lives.
3. **Engines are external processes we observe and supervise**, not libraries we fork or
   patch (see [ADR-005](decisions.md#adr-005)). We start/stop/health-check them and read
   their APIs. We do not reach into their memory allocators.
4. **Coarse, not clever.** Residency is process-level (load/unload). No VRAM preemption,
   no cross-engine memory fairness (see [ADR-001](decisions.md#adr-001)).

## Component map

```
              Phone / Browser
                    │
        Tailscale Serve  /  LAN (127.0.0.1)
                    │
   ┌────────────────▼─────────────────┐
   │  Web UI (bundled static assets)  │   GPU • models • queue • engines • logs
   └────────────────┬─────────────────┘
                    │  REST + SSE
   ┌────────────────▼─────────────────┐
   │        Control Plane (daemon)    │
   │  ┌────────────┬────────────────┐ │
   │  │ Inventory  │ Compatibility  │ │  ← the differentiators
   │  │ Manifests  │ Estimator      │ │
   │  ├────────────┼────────────────┤ │
   │  │ Residency  │ Metrics        │ │
   │  │ Manager    │ Collector      │ │
   │  └─────┬──────┴───────┬────────┘ │
   │        │ SQLite (WAL) │          │
   └────────┼──────────────┼──────────┘
            │              │
     Engine adapters   Hardware probe
            │           (NVML/ROCm/psutil)
   ┌────────┼──────────┬─────────────┐
   ▼        ▼          ▼
 Ollama   llama.cpp   vLLM
 daemon   (we own     (we own the
 (shared) each proc)   proc/container)
```

### Control plane

Long-running daemon. Owns SQLite, exposes the REST + SSE API, runs the metrics loop,
supervises engine processes it launched (llama.cpp, vLLM), and talks to the shared
Ollama daemon over HTTP.

### Engine adapters

One module per engine implementing a common contract (see
[`engine-adapters.md`](engine-adapters.md)). The key asymmetry:

- **Ollama** is a *shared* daemon we do not own. We influence it via HTTP + env vars.
- **llama.cpp / vLLM** processes are *owned* by LocalHub — we spawn, supervise, and kill
  them. This is the llama-swap-shaped part of the system.

### Hardware probe

Reads GPU/CPU/RAM/disk. NVIDIA-first via NVML (`go-nvml` or `pynvml`); ROCm via
`rocm-smi`/`amd-smi`; CPU/RAM/disk via gopsutil/psutil. Feeds both the live dashboard and
the compatibility engine (which needs *current free VRAM*, not just total).

### Compatibility engine + estimator

The core UX. Given a model's metadata and the current hardware, produces a structured
verdict. Estimation delegates to `gguf-parser-go` for GGUF and to our own math for the
safetensors/vLLM path. Full spec in
[`compatibility-engine.md`](compatibility-engine.md).

### Residency manager

Admission control + eviction over the set of running instances. Coarse and conservative.
Full spec in [`residency-scheduler.md`](residency-scheduler.md).

## Process/state model

- **Ollama models:** state lives in the Ollama daemon; we mirror it via `/api/ps`.
- **llama.cpp / vLLM instances:** LocalHub owns a supervised child process per instance,
  tracked in the `instances` table with PID, port, and lifecycle state.
- On control-plane restart, reconcile: re-probe Ollama `/api/ps`, re-attach or reap
  orphaned child processes recorded in `instances`.

State machine per instance:

```
REQUESTED → STARTING → HEALTHY → (DRAINING) → STOPPED
                 │          │
                 └── FAILED ┘
```

`DRAINING` = marked for eviction but has in-flight requests; do not kill until drained
(see residency rules).

## Stack decision (read this before choosing a language)

The plan proposed "Python/FastAPI for prototype, Go single-binary later." For a **real
FOSS product whose adoption depends on install friction**, the distribution story is not
a "later" concern — it is the product. Two honest options:

### Option A — Go control plane (recommended for the product)

- **Single static binary.** No Python runtime, no venv, no CUDA in the control plane.
  `curl | tar | ./localhub`. This is the single biggest adoption lever.
- **`gguf-parser-go` is a Go library** — import it directly, no subprocess/JSON hop.
- **`tsnet`** embeds Tailscale directly (see [security.md](security.md)).
- Bundle the built frontend as embedded static assets.
- Cost: Go is slower to prototype than FastAPI; HF metadata parsing is nicer in Python.

### Option B — Python/FastAPI control plane (faster to a working demo)

- Fastest path to Phase 1–2, especially given existing FastAPI familiarity.
- `pynvml`, `huggingface_hub`, `safetensors` are all first-class in Python.
- Must shell out to the `gguf-parser` binary (fine — it has a clean `--json` mode).
- Cost: install friction (Python + deps). Mitigate by shipping a Docker image *and* a
  PyInstaller/`uv`-based single-command install, but it will never be as clean as a Go
  binary.

**Recommendation:** if you're serious about "everyone can use it," bias to **Go for the
control plane** and keep a small optional Python helper only if HF parsing gets painful.
If you want a working artifact this month to validate the UX, start in **Python** and
treat a Go rewrite of the daemon as a real milestone, not a someday. Either way: the
**frontend is React/Vite**, the **DB is SQLite (WAL)**, and adapters shell out to engine
CLIs/HTTP. Decide explicitly in [ADR-006](decisions.md#adr-006) and don't relitigate it
every phase.

## Data flow: a request end-to-end (Phase 6)

```
client → /v1/chat/completions (thin proxy)
      → resolve model_id → target engine + instance
      → residency manager: warm? else admit (estimate → maybe evict → start)
      → route to engine's OpenAI endpoint (Ollama / llama-server / vLLM)
      → stream tokens back
      → record tokens/s, TTFT, latency, memory delta
```

Before Phase 6, there is **no proxy** — LocalHub only manages/monitors, and clients hit
the engines directly. Ship value without the proxy first.
