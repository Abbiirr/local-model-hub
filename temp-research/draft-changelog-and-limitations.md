# Draft CHANGELOG + KNOWN LIMITATIONS — LocalHub Compass v0.1.0

Staging draft for the C1 close-out. Every line is grounded in the dated evidence files
(P0 friction log, P2.5 validation matrix, P5.3 load-storm, lmwrapper reuse notes, wiring
notes). Promote to `CHANGELOG.md` and `KNOWN-LIMITATIONS.md` (or README sections) at
release. **No marketing language** — user-facing verbs only, each claim traceable.

---

## CHANGELOG

### v0.1.0 — first MVP (unreleased)

Single-box control plane for local LLMs across Ollama, llama.cpp, and vLLM. Tells you
which models run on your exact GPU, on which engine, at what flags — then loads, stops,
and queues them safely, and serves a live dashboard to your phone over Tailscale.

Reference box for all validation: RTX 4060 Ti 16 GB (CC 8.9), driver 576.40, 16 GB RAM,
Windows 10. Engines: Ollama 0.21.2, llama.cpp (CUDA fork turbo-tan/llama.cpp-tq3 commit
1cfa891 + winget build 8833 Vulkan), vLLM via Docker (WSL2). Estimator: gguf-parser-go
v0.24.1.

**Inventory & observability (Phase 1)**
- Unified inventory across Ollama models, loose GGUF folders, and HF/safetensors
  directories in one normalized table (format, quant, size, hash where available).
  Re-scan is idempotent.
- Hardware probe (via `nvidia-smi`): GPU name, total/free VRAM, driver, compute
  capability, utilization, power, temperature, plus CPU/RAM/disk. Reports the **real**
  free VRAM, not nominal total (the desktop already consumes ~2.8 GB on the reference box).
- Read-only engine detection for Ollama (`/api/version`, `/api/tags`, `/api/ps`) and
  configured llama.cpp / vLLM entries — zero mutation of engine state.
- React dashboard served by the daemon (no external CDN): live GPU panel, engines,
  inventory. Live updates over both SSE and a WebSocket channel.
- Phone access over Tailscale Serve (localhost bind + Serve proxy; identity headers
  trusted only behind localhost).

**Compatibility engine (Phase 2)**
- Pre-flight `runs` / `runs_degraded` / `wont_run` verdict per model + hardware, each
  carrying a mandatory human-readable `reason`, `estimated_vram`, `max_context_that_fits`,
  and `run_flags`.
- GGUF VRAM estimates via gguf-parser-go (library, not reinvented); local files and
  **remote HF repos read by HTTP range request — no weight download**.
- Safetensors/vLLM estimator: weights + GQA-aware KV cache + fixed overhead from
  `config.json` / safetensors index; reports the minimum `--gpu-memory-utilization` that
  holds the model.
- Graceful fallback for quant types the parser cannot read (e.g. TQ3 → GGMLType 46): a
  clearly-labelled, conservative size+KV heuristic instead of a hard failure or a false
  `wont_run`.
- `POST /api/compat/preview` returns a verdict for a remote repo without downloading.
- Measured-beats-estimate calibration: after a real load, the observed NVML VRAM peak is
  stored and returned verbatim (`confidence: exact`) on later verdicts.
- Validated against a 21-model matrix (GGUF + safetensors, GQA + MHA, fits + doesn't):
  **zero false-positive `runs`**.

**Supply chain (Phase 3)**
- Hugging Face downloads resolve branch/tag → exact commit SHA and show it (with the
  verdict) **before** committing to download; resumable; per-file SHA-256 recorded and
  verified.
- Single-GGUF URL path, Ollama pull passthrough (records digest), and local-import path
  (with an explicit `local-import` provenance warning).
- Every completed download/import writes an **immutable manifest** (source, revision/
  commit, file list, hashes, license, warnings). Reproduce export names repo + commit +
  files + hashes (+ launch flags if run).
- Pickle / `trust_remote_code` / no-license / gated-repo produce **warnings, not blocks**
  by default (strict mode optional).

**Controlled run/stop (Phase 4)**
- Instance state machine (REQUESTED → STARTING → HEALTHY → DRAINING → STOPPED/FAILED)
  persisted to SQLite; startup reconciliation reattaches, marks-stopped, or reaps orphans.
- llama.cpp adapter: spawns `llama-server` (port pool, flags recorded, localhost bind),
  health-checks, stops, and **verifies VRAM reclaimed via NVML delta** — not an HTTP 200.
  Capability-probes `--help` to route TQ3 models to the CUDA fork.
- Ollama adapter: warms via API, mirrors `/api/ps`, unloads via `keep_alive: 0`, verifies
  by polling — never kills the shared daemon process.
- vLLM adapter: Docker and subprocess launch modes against the documented CLI/HTTP
  contract; STARTING state surfaced; command strings unit-tested string-exactly.
- One controlled real vLLM Docker E2E session (WSL2, tiny model) exercised start → health
  → completion → stop with VRAM returned to baseline.

**Coarse residency (Phase 5)**
- Admission control: warm-check → estimate → read live free VRAM → evict-if-safe → start,
  else queue or reject with a `blocked_reason`. Whole-model load/unload only (no VRAM
  preemption — ADR-001). Free VRAM re-read from the probe after every eviction.
- Custom priority queue: three classes (interactive/normal/background), aging to prevent
  starvation, size/cost-aware tie-break, bounded same-model coalescing with a higher-class
  guard, per-class caps (64/256/512) → `queue_full` rejection with a numeric reason.
  Documented in docs/residency-priority-queue.md; property-tested.
- Eviction never selects pinned or active instances by default; DRAINING semantics honored.
- Load-storm on the reference GPU: more requests than fit → no OOM, no killed active
  stream, no crash, correct queue/refusal reasons (see Known Limitations for the honest
  scope of what was exercised live).

**Cross-cutting**
- Every API error returns `{ error: { code, message, reason? } }` — no opaque 500s.
- Binds `127.0.0.1` by default; remote access only via Tailscale Serve.
- Single Go binary; SQLite (WAL); React/Vite frontend embedded.

**Decisions recorded (ADRs)**
- ADR-003 resolved: build the custom residency manager (do not drive llama-swap) — the
  swap trial proved llama-swap has no VRAM-aware admission, estimates, priority, or
  reasons.
- ADR-006 accepted: Go control plane.

**Not in this release (by design — see product-brief non-goals)**
- No cross-engine VRAM preemption / fair-share scheduler (ADR-001).
- No automatic Safetensors→GGUF conversion in core (ADR-004).
- No chat UI, no multi-provider gateway/router, no k8s/cluster, no auth/billing.

---

## KNOWN LIMITATIONS

Ordered roughly by how likely a user is to hit them. Every item cites its evidence.

### 1. TQ3 (and other unparseable quants) fall back to a conservative heuristic — a false `wont_run` is possible
gguf-parser-go v0.24.1 **cannot parse TQ3 quants** (fails with `GGMLType(46) unsupported`,
even for metadata). LocalHub degrades to a labelled file-size + KV heuristic rather than
erroring, but that heuristic is deliberately conservative and **over-counts**. Concretely,
the four TQ3 flagship models on the reference box (Qwen3.x 27B/35B) get `wont_run` verdicts
(e.g. Qwen3.6-27B-TQ3_4S estimated ~16178 MiB @8K vs 12226 usable) — yet the battle-tested
`run-qwen3.6-27b-tq3_4s.bat` profile **does run them** at low context when VRAM is freer.
The verdict carries the fallback caveat and notes a measured peak may fit. **Mitigation:**
load it once for real; the observed-peak calibration then returns an exact verdict
thereafter. (research/phase-2-validation-matrix.md rows 9–12; friction-log #1; wiring-p21.)

### 2. vLLM footprint is `gpu-memory-utilization × total VRAM`, so co-residency needs a lowered util
vLLM **pre-allocates** `--gpu-memory-utilization × TOTAL` VRAM and fills it — it does not
size to the model. The estimator reports the required VRAM and the minimum util that holds
it, and the adapter passes a recommended util (defaulting up to 0.90). But if you want a
vLLM model to co-reside with another engine's model, you must **lower vLLM's util
explicitly** or vLLM will grab its whole configured fraction of the card regardless of the
model's actual size. (wiring-p22 §3; safetensors estimator `GPUMemUtil`.)

### 3. ~3 concurrent engine processes is the real ceiling on a 16 GB-RAM box (RAM, not VRAM, binds first)
In the load-storm, **four resident `llama-server` processes drove free system RAM to
0.24–0.54 GB** (on the 16 GB box, with Docker Desktop/other apps also resident), tripping
the 2 GB RAM safeguard and aborting the storm — **while VRAM never OOMed** (min free 1086
MiB). Admission currently budgets **VRAM only**; it does not model host-RAM pressure. On a
16 GB-RAM machine, plan for ~3 concurrent llama.cpp processes, not more.
(research/phase-5-load-storm.md §"Not exercised" #1.)

### 4. WSL2 9p mount makes model weight loading slow
Loading model weights from a Windows path into the vLLM Docker container over the WSL2 9p
filesystem is slow (one observed weight load ~79.9 s). Keep model files on a Linux-native
path inside WSL, or a mounted volume with better throughput, for faster vLLM cold starts.
*(P4.5 controlled-session finding.)*

### 5. Cancellation mid-decode is best-effort — the engine finishes the token stream
A client disconnect does **not** cancel an upstream llama.cpp generation; it runs to
completion. Mid-decode interruption is effectively impossible on the engines LocalHub
supervises, so cancellation is honored cleanly only while a request is still **queued** (or
during prefill); once decode is underway, the engine completes it and LocalHub stops
tracking, but does not abort the engine. Eviction is therefore designed around idle-only.
(research/lmwrapper-reuse-notes.md §"Documented real-world lessons"; gaps_and_remaining.)

### 6. Ollama VRAM accounting is an approximation (shared daemon, 30 s active window)
Ollama is a **shared daemon** LocalHub does not own. VRAM for Ollama models is read from
`/api/ps` (`size_vram`), which disagreed with both gguf-parser (579 MiB) and the NVML delta
(701 MiB) for the same model — three numbers, ~30–60 % apart; the measured NVML delta is
treated as truth. "Active" for an Ollama model is approximated by a recent-activity window
(~30 s) rather than exact in-flight tracking, and unload is requested via `keep_alive: 0`
and confirmed by polling (never a process kill). (matrix row 6; friction-log #6/#8.)

### 7. Hardware probe is Windows/`nvidia-smi`-only today
The current hardware probe shells out to `nvidia-smi` (the control plane deliberately does
not link CUDA/NVML — ADR-005). Native NVML and ROCm/`amd-smi` probes are designed as
alternate implementations of the same interface but **are not shipped**; this release is
validated on NVIDIA + Windows only. `nvidia-smi` parsing stability across driver versions
is a maintenance risk. (PLAN.md decision 5; docs/23 machine profile.)

### 8. Remote-HF vLLM start has a UX gap (no download progress surfaced)
When a vLLM model is started from an HF repo id, the id is passed to `--model` and vLLM
**downloads the weights inside the container on first start** — LocalHub surfaces STARTING
but does not surface the in-container download progress, so a first start of a large remote
model can sit in STARTING for a long time with no visible reason. Pre-caching the HF weights
(mounted `~/.cache/huggingface`) avoids this. *(P4.5-class finding; command.go passes the
HF repo id straight to `--model`.)*

### 9. `--model` in the vLLM image is a version-drift risk (flagged)
The vLLM adapter's docker invocation passes `--model <repo>` after the image name. The exact
form of vLLM's model argument has drifted across versions; the command is unit-tested
string-exactly so a break is caught by a failing test, but a vLLM image upgrade may require
updating the flag. Pin the vLLM image and re-verify the launch contract before upgrading.
(internal/engines/vllm/command.go RE-VERIFY note; docs/22 version policy.)

### 10. No authentication — the trust model is localhost + Tailscale
There is **no built-in auth**. Admin controls are protected only by (a) the default
`127.0.0.1` bind and (b) Tailscale Serve + tailnet ACLs / identity headers for remote
access. An optional `admin_token` exists in config but is null by default. Do not expose
LocalHub directly to the public internet; there is no Funnel-safe mode. (docs/security.md;
config.example.yaml `admin_token: null`.)

### 11. Live eviction and mid-stream survival not yet proven on real hardware (unit/property-tested only)
The load-storm resolved by **queueing**, not eviction, so the live `evict_then_load` path
and its per-eviction VRAM re-read were **not triggered on hardware** (proven by unit/
property tests only). And the pinned stream that survived the storm had **finished ~15 s
before** admission pressure arrived, so "an active in-flight stream survives concurrent
eviction" was not exercised as a live race. Additionally, `InFlightTracker` has **no
production callers** — a **non-pinned** streaming instance is currently invisible to the
never-kill-active gate and is protected only if pinned (or within Ollama's recent-active
window). Phase 6's proxy must own stream counting to close this. Until then, treat
"never kill an active stream" as guaranteed for **pinned** instances only.
(research/phase-5-load-storm.md §"A stream integrity", §"Phase 6 notes".)

### 12. No production queue-drain loop yet — queued requests do not auto-start when VRAM frees
`queue.Next()` is not called outside tests; stopping/evicting an instance frees VRAM but
does not re-attempt a queued start, and there is no TTL/periodic drain tick. A queued
request therefore **will not auto-start** when room frees — a re-POST is required. This is
unimplemented (not merely untested); the clean fix is a `core.Service.DrainQueue`, likely
folded into the Phase 6 persisted job queue. (research/phase-5-load-storm.md §"Not
exercised" #2.)

### 13. Phone-over-Tailscale confirmation is pending the user
All agent-preparable phone-access work is done (serve script, SSE proxy-friendly headers,
loopback bind, docs), but on the reference box the Tailscale backend wedged in NoState and a
**restart needs an elevated shell**. The final phone check (open the ts.net dashboard on a
phone) is a **manual user action** and has not been performed. (TESTING.md#p16.)
