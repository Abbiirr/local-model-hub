# lmwrapper_project reuse notes (mined 2026-07-06, Opus explorer)

Source: `K:\projects\llm_models\lmwrapper_project` (Python, working V1 control-plane
package + legacy live monolith) and `K:\projects\llm_models\llama.cpp-tq3` (fork).
These notes feed LocalHub Compass design: `internal/residency`, `internal/engines/llamacpp`,
`internal/manifest`, `internal/compat`.

## What it is

Single-box Ollama-replacement gateway on 127.0.0.1:11435 for this exact machine
(16 GB VRAM / 16 GB RAM Windows box). Two generations:
- `ollama_wrapper_server.py` — legacy live monolith (~2650 lines, FastAPI, global
  inference lock). Use only as an endpoint checklist.
- `lmwrapper/` package — clean modular control plane (allocator, scheduler, worker,
  runtime, validation, telemetry, artifact). 105 static tests pass; fake-worker
  acceptance 75 tests; real gateway smoke 10/10. **Port these algorithms.**

## Algorithms to port into Go

1. **Allocator decision enum** (`lmwrapper/allocator.py:60-167`):
   `route_loaded | load_new | evict_then_load | queue | reject`.
   - Evicts ONLY workers in `{ready_warm, idle}` with `active_requests==0`;
     busy blockers → `queue`, never evict active. (= our never-kill-streams gate.)
   - `execute_eviction` re-checks idle at execution time and confirms memory actually
     freed (`wait_memory_freed`) before success. (= our NVML-delta rule.)
2. **Priority scheduler** (`lmwrapper/scheduler.py`):
   - 3 tiers: interactive=0 < normal=1 < background=2; heap key `(priority, created_at)`
     → strict priority, FIFO within tier.
   - Per-priority queue caps {64/256/512} → immediate `queue_full` rejection (429).
   - Priority derivation: chat/generate→interactive; embeddings/tokens→background;
     override via header `X-LMW-Priority` → metadata → endpoint default.
   - **Bounded same-profile coalescing with starvation guard** (`next_for_profile`,
     `drain_same_profile`, max 4): drain queued jobs for the already-loaded model to
     avoid A/B/A reload thrash — but only if no higher-priority job waits.
     ← single most valuable idea; core of our P5.0 custom algorithm.
   - Wait-budget timeout eviction from queue; queued-cancel clean; in-flight cancel
     only during prefill (optional emergency kill).
3. **Measured-VRAM fit** (`allocator.py:40-57`): required = observed_peak_vram_mib
   (from a real validation run via nvidia-smi) + margin (768 MiB VRAM / 2048 MiB RAM;
   +10% fragmentation margin when colocated). Unvalidated + gpu_strict → reject.
   ← Compass verdicts are estimate-based pre-download (gguf-parser), but post-first-run
   we should record observed peaks and prefer them (estimator error loop, P2.5).
4. **Capability probing** (`runtime.py:54-62`): grep backend `--help`/`--version` for
   flags before using them (`--fit`, `-ctk`, `--no-warmup`, "tq3"/"turboquant").
   ← llama.cpp adapter MUST do this: fork-only vs upstream flags.
5. **Artifact manifest** (`artifact.py`): sha256 per file, record
   {id, kind, source, path, size, hash}, path containment validation. → internal/manifest.
6. **Validation cache invalidation** (`validation.py:54`, `docs/validation_cache.md`):
   key includes binary hash, model hash, GPU UUID, driver, ctx, KV type, batch, parallel.
   → compat verdict cache keying.
7. **Worker lifecycle** (`worker.py`): 14 states; Windows process rules:
   `CREATE_NO_WINDOW`, terminate() then kill() after 5 s; memory-free polling before
   declaring evicted.
8. **nvidia-smi telemetry** (`telemetry.py:65`): `--query-gpu` + `--query-compute-apps`
   subprocess — exactly the probe design PLAN.md decision 5 chose.
9. **Runtime profile schema** (`profiles/schema/runtime_profile.schema.json`):
   `execution_policy` = gpu_strict | gpu_preferred | hybrid_allowed, fallback_profiles,
   allow_context_downgrade. Strict mode bans `--n-cpu-moe`, `--tensor-split`,
   `--no-kv-offload` (`runtime.py:11 UNSAFE_STRICT_ARGS`).

## llama.cpp-tq3 fork facts (adapter-critical)

- Fork of llama.cpp: github.com/turbo-tan/llama.cpp-tq3 (custom TQ3 CUDA quant kernels:
  tq3-native.cu, mmq-instance-tq3_*.cu). Local models Qwen3.x-TQ3_* need THIS binary.
- **CORRECTION (P0.2 field trial, 2026-07-06):** `--fit`, `--sleep-idle-seconds`, and
  `--models-dir` are now in UPSTREAM llama.cpp too (winget b8833 accepts them).
  Genuinely fork-only: the **`tq3_0` KV cache type + TQ3_* quant kernels + this
  build's CUDA backend** (the winget build is Vulkan/CPU-only — no CUDA). Capability
  probing remains mandatory: gate `-ctv tq3_0` and TQ3 model routing on detecting
  "tq3"/"turboquant" in `--help`; gate CUDA-dependent expectations on backend probe.
- **gguf-parser CANNOT parse TQ3_* quants** (fails with GGMLType(46) even on metadata).
  The compat engine needs a documented fallback for unparseable quant types
  (file-size + overhead heuristic, clearly labeled low-confidence, verdict reason says
  why) — feeds P2.1/P2.3.
- Known-good launch profile (run-qwen3.6-27b-tq3_4s.bat): `-ngl all -c CTX -np 1
  --fit off -ctk q4_0 -ctv tq3_0 -fa on --sleep-idle-seconds N --no-warmup --jinja`.

## Documented real-world lessons (don't relearn these)

- Restart is not transparent: queued HTTP requests and in-flight generations don't
  survive restarts; only restart when active==null and depth==0
  (docs/restart_queue_semantics.md). → our P6.2 persisted job queue must persist the
  *job records*, and reconciliation must not pretend streams survived.
- Client disconnect does NOT cancel upstream llama.cpp generation — it runs to
  completion (gaps_and_remaining.md:44). Budget for this in residency "active" logic.
- Mid-decode interruption is effectively impossible; design eviction around idle-only.
- Duplicate `ollama serve` listeners are a real hazard; cold-start scripts `ollama stop`
  every running model then wait ~5 s for VRAM release.
- Single global inference serialization was a deliberate 16 GB safety choice
  (max_loaded_local_workers != 1 flagged unsafe).
- Reject 1×1/tiny images — llama.cpp has crashed on them (vision path).
- No JSON-RPC prior art anywhere in the project (WebSocket is event-framed
  `{"type": ...}` with lifecycle events request.accepted/queue.update/model.loading/
  model.ready/response.delta/response.completed/heartbeat) — reuse that event
  vocabulary for our WS channel; JSON-RPC 2.0 layer is net-new work.

## API endpoint checklist worth mirroring (from legacy monolith)

/v1/chat/completions, /v1/completions, /v1/embeddings, /v1/models (OpenAI);
WS /ws/v1/chat with lifecycle events; structured "unsupported" responses for
unimplemented OpenAI params rather than silent drops.
