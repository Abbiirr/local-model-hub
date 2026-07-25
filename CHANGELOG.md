# Changelog

All notable changes to LocalHub Compass are recorded here. No marketing language — every
line is a user-facing capability traceable to the code and the dated evidence files.

## v0.1.0 — first MVP (unreleased)

Single-box control plane for local LLMs across Ollama, llama.cpp, and vLLM. Tells you which
models run on your exact GPU, on which engine, at what flags — then loads, stops, and queues
them safely, serves a live dashboard to your phone over Tailscale, and exposes a thin
OpenAI-compatible proxy + durable async jobs + a JSON-RPC 2.0 control surface.

Reference box for all validation: RTX 4060 Ti 16 GB (CC 8.9), driver 576.40, 16 GB RAM,
Windows 10. Engines: Ollama 0.21.2, llama.cpp (CUDA fork turbo-tan/llama.cpp-tq3 commit
1cfa891 + winget build 8833 Vulkan), vLLM via Docker (WSL2). Estimator: gguf-parser-go
v0.24.1.

### Inventory & observability (Phase 1)

- Unified inventory across Ollama models, loose GGUF folders, and HF/safetensors directories
  in one normalized table (format, quant, size, hash where available). Re-scan is idempotent.
- Hardware probe (via `nvidia-smi` subprocess): GPU name, total/free VRAM, driver, compute
  capability, utilization, power, temperature, plus CPU/RAM/disk. Reports the **real** free
  VRAM, not nominal total (the desktop already consumes ~2.8 GB on the reference box).
- Read-only engine detection for Ollama (`/api/version`, `/api/tags`, `/api/ps`) and
  configured llama.cpp / vLLM entries — zero mutation of engine state.
- React dashboard served by the daemon (no external CDN): live GPU panel, engines, inventory.
  Live updates over both SSE (`/api/hardware/live`) and a WebSocket channel (`/api/ws`).
- Phone access over Tailscale Serve (localhost bind + Serve proxy; identity headers trusted
  only behind localhost).

### Compatibility engine (Phase 2)

- Pre-flight `runs` / `runs_degraded` / `wont_run` verdict per model + hardware, each carrying
  a mandatory human-readable `reason`, `estimated_vram`, `max_context_that_fits`, and
  `run_flags`.
- GGUF VRAM estimates via gguf-parser-go (library, not reinvented); local files and **remote
  HF repos read by HTTP range request — no weight download**.
- Safetensors/vLLM estimator: weights + GQA-aware KV cache + fixed overhead from `config.json`
  / safetensors index; reports the minimum `--gpu-memory-utilization` that holds the model.
- Graceful fallback for quant types the parser cannot read (e.g. TQ3 → `GGMLType(46)`): a
  clearly-labelled, conservative size+KV heuristic instead of a hard failure or a false
  `wont_run`.
- `POST /api/compat/preview` returns a verdict for a remote repo without downloading.
- Measured-beats-estimate calibration: after a real load, the observed NVML VRAM peak is
  stored and returned verbatim (`confidence: exact`) on later verdicts.
- Validated against a 21-model matrix (GGUF + safetensors, GQA + MHA, fits + doesn't): **zero
  false-positive `runs`**.

### Supply chain (Phase 3)

- Hugging Face downloads resolve branch/tag → exact commit SHA and show it (with the verdict)
  **before** committing to download; resumable; per-file SHA-256 recorded and verified.
- Single-GGUF URL path, Ollama pull passthrough (records digest), and local-import path (with
  an explicit `local-import` provenance warning).
- Every completed download/import writes an **immutable manifest** (source, revision/commit,
  file list, hashes, license, warnings). Reproduce export names repo + commit + files + hashes
  (+ launch flags if run).
- Pickle / `trust_remote_code` / no-license / gated-repo produce **warnings, not blocks** by
  default (strict mode optional).

### Controlled run/stop (Phase 4)

- Instance state machine (REQUESTED → STARTING → HEALTHY → DRAINING → STOPPED/FAILED)
  persisted to SQLite; startup reconciliation reattaches, marks-stopped, or reaps orphans.
- llama.cpp adapter: spawns `llama-server` (port pool, flags recorded, localhost bind),
  health-checks, stops, and **verifies VRAM reclaimed via NVML delta** — not an HTTP 200.
  Capability-probes `--help` to route TQ3 models to the CUDA fork.
- Ollama adapter: warms via API, mirrors `/api/ps`, unloads via `keep_alive: 0`, verifies by
  polling — never kills the shared daemon process.
- vLLM adapter: Docker and subprocess launch modes against the documented CLI/HTTP contract;
  STARTING state surfaced; command strings unit-tested string-exactly.
- One controlled real vLLM Docker E2E session (WSL2, tiny model) exercised start → health →
  completion → stop with VRAM returned to baseline.

### Coarse residency (Phase 5)

- Admission control: warm-check → estimate → read live free VRAM → evict-if-safe → start, else
  queue or reject with a `blocked_reason`. Whole-model load/unload only (no VRAM preemption —
  ADR-001). Free VRAM re-read from the probe after every eviction.
- Custom priority queue: three classes (interactive/normal/background), aging to prevent
  starvation, size/cost-aware tie-break, bounded same-model coalescing with a higher-class
  guard, per-class caps (64/256/512) → `queue_full` rejection with a numeric reason. Documented
  in `docs/residency-priority-queue.md`; property-tested.
- Eviction never selects pinned or active instances by default; DRAINING semantics honored.
- Load-storm on the reference GPU: more requests than fit → no OOM, no killed active stream,
  no crash, correct queue/refusal reasons (see KNOWN-LIMITATIONS for the honest scope of what
  was exercised live).

### Thin OpenAI proxy · async jobs · JSON-RPC (Phase 6)

- **Thin OpenAI proxy** (`internal/proxy`, `internal/api/proxy.go`): `GET /v1/models` lists
  present + runnable inventory in OpenAI list shape (a `wont_run` model is never listed);
  `POST /v1/chat/completions` and `POST /v1/completions` resolve the `model` name, run the full
  P5 admission (warm / load / evict+load / queue / reject), and stream tokens back with
  **byte-passthrough** (response chunks relayed exactly, no SSE re-parse). Blocked admission
  returns a fast OpenAI-shaped `503` carrying the numeric `blocked_reason`. Every proxied
  request goes through the counted relay, so a non-pinned instance serving a proxied stream is
  visible to the never-kill-active gate; client disconnect aborts the stream, not the instance.
  vLLM dev endpoints are never forwarded.
- **Async inference jobs** (`internal/core/jobs.go`, migration `0007_jobs.sql`): `POST
  /api/jobs` returns an id immediately; the job runs through the same residency admission,
  relays non-streaming, and stores the final OpenAI response in `result_json`. State machine
  `queued → running → succeeded | failed | cancelled`. Durable — queued jobs survive a daemon
  restart; a `running` job on restart becomes `failed` ("daemon restarted mid-inference"),
  never silently re-run. Cancellation is honest (mid-queue removes the queue row; mid-inference
  aborts the relay but does not kill the instance). Job state changes publish `jobs.updated`
  over `/api/ws`. Every failure/cancel carries a non-empty `reason`.
- **JSON-RPC 2.0 surface** (`internal/jsonrpc`, `POST /api/rpc` + `/api/ws` frames): a thin
  layer over the same `core.Service` — every method a one-line core call, zero business logic.
  Registry mirrors the REST core (hardware / engines / models / compat / instances / queue /
  jobs). Spec-compliant framing (id echo, notifications, batch) and error objects
  (`-32700/-32600/-32601/-32602/-32000`) that preserve the human `reason` in `data.reason`.
- **Auth middleware enforced** (`internal/api/middleware.go`): when `server.admin_token` is
  set, `/api/*` and `/v1/*` require `Authorization: Bearer <token>` (constant-time compare);
  `GET /api/health` and static UI assets exempt.
- Live-verified 2026-07-07 against a real small llama.cpp model within hardware safeguards
  (`research/phase-6-live-gates.md`). One sub-criterion is not yet met: the blocked-admission
  path measures ~10 s rather than the intended <2 s (synchronous compat estimate ahead of
  admission) — the 503 contract itself is fully correct; fix in progress. See KNOWN-LIMITATIONS.

### Cross-cutting

- Every internal API error returns `{ error: { code, message, reason? } }` — no opaque 500s.
  The `/v1/*` proxy uses OpenAI's error dialect but preserves the reason in `message`.
- Binds `127.0.0.1` by default; remote access only via Tailscale Serve.
- Single Go binary; SQLite (WAL); React/Vite frontend embedded.

### Decisions recorded (ADRs)

- ADR-003 resolved: build the custom residency manager (do not drive llama-swap) — the swap
  trial proved llama-swap has no VRAM-aware admission, estimates, priority, or reasons.
- ADR-006 accepted: Go control plane.

### Not in this release (by design — see product-brief non-goals)

- No cross-engine VRAM preemption / fair-share scheduler (ADR-001).
- No automatic Safetensors→GGUF conversion in core (ADR-004).
- No chat UI, no multi-provider gateway/router (that's LiteLLM), no k8s/cluster, no
  auth/billing beyond the single optional admin token.
