# LocalHub Compass — Spec Digest (implementer briefing)

Primary briefing for implementer subagents. Dense restatement of the spec docs so you do
NOT need to re-read them. Citations point back to the source doc/section. Where a doc claim
is time-sensitive (engine behavior), it is flagged **[RE-VERIFY]** — you must re-check the
engine's current primary source before relying on it (CLAUDE.md discovery rule).

Reference box (all gates close here): Windows 10 (19045), PowerShell + Git Bash, NOT a git
repo; RTX 4060 Ti 16 GB (CC 8.9, driver 576.40); 16 GB system RAM; storage on `K:`. Ollama
+ llama.cpp present natively; **vLLM absent, no native Windows — Linux-only via Docker/WSL2**
(PLAN.md). Hard hardware safeguards apply to every task (PLAN.md "Hardware safeguards").

---

## 1. ARCHITECTURE (architecture.md, 16-implementation-playbook.md)

**Design constraints, priority order** (architecture.md §Design constraints):
1. Single box first — no cluster assumptions in data model/API/scheduler [ADR-007].
2. Control plane NEVER links/touches CUDA — it shells out to engines. This is what makes it
   a single static binary.
3. Engines are external processes we observe/supervise, not libraries we fork/patch [ADR-005].
4. Coarse, not clever — residency is process load/unload; no VRAM preemption/fairness [ADR-001].

**Components an implementer must create** (architecture.md §Component map; playbook §Repo shape):
- **Control plane daemon** (`cmd/localhub/`) — long-running; owns SQLite; exposes REST+SSE;
  runs metrics loop; supervises spawned engine processes; talks to shared Ollama over HTTP.
- **Engine adapters** (`internal/engines/{ollama,llamacpp,vllm}/`) — one module per engine,
  common contract. Ownership asymmetry is load-bearing (see §5).
- **Hardware probe** (`internal/hardware/`) — GPU/CPU/RAM/disk. Feeds dashboard AND compat
  engine (needs CURRENT free VRAM, not just total). PLAN.md decision 5: probe is an
  interface with two impls — `nvidia-smi` subprocess now (Windows), native NVML later (Linux).
- **Compatibility engine + estimator** (`internal/compat/`) — the core UX; produces structured
  verdict. GGUF via `gguf-parser-go`; safetensors/vLLM via own math.
- **Residency manager** (`internal/residency/`) — admission control + eviction, coarse.
- **Manifest/supply-chain** (`internal/manifest/`) — provenance + hashing.
- **Metrics collector** (`internal/metrics/`) — collectors + Prometheus output.
- **API layer** (`internal/api/`) — REST + SSE handlers.
- **DB layer** (`internal/db/`) — migrations + queries.
- **Web UI** (`web/`) — React + Vite, bundled as embedded static assets.

**Stack** (ADR-006 → resolved to **Go** in PLAN.md decision 1): single static binary,
`gguf-parser-go` imported natively, React+Vite embedded, SQLite WAL. Frontend=React/Vite,
DB=SQLite(WAL) regardless of language.

**Process/state model** (architecture.md §Process/state model):
- Ollama models: state lives in Ollama daemon; mirror via `/api/ps`.
- llama.cpp/vLLM: LocalHub owns one supervised child process per instance, tracked in
  `instances` (PID, port, state).
- On control-plane restart: reconcile — re-probe Ollama `/api/ps`; re-attach or reap orphaned
  child processes recorded in `instances`. (Reconcile on EVERY daemon startup — playbook rule.)

**Instance state machine** (architecture.md):
`REQUESTED → STARTING → HEALTHY → (DRAINING) → STOPPED`; `STARTING`/`HEALTHY` can go `FAILED`.
`DRAINING` = marked for eviction but has in-flight requests; do not kill until drained.

**Data flow, request end-to-end (Phase 6 only)**: client → `/v1/chat/completions` (thin
proxy) → resolve model_id → target engine+instance → residency admit (warm? else estimate →
maybe evict → start) → route to engine OpenAI endpoint → stream tokens back → record
tokens/s, TTFT, latency, memory delta. **Before Phase 6 there is NO proxy** — clients hit
engines directly.

**First code milestone** (playbook §First code milestone): expose ONLY `GET /api/hardware`,
`GET /api/hardware/live` (SSE), `GET /api/engines`, `GET /api/models`, SQLite migration
bootstrap, minimal React page. **No model start/stop in first milestone.**

**Coding rules** (playbook §Coding rules): no control-plane CUDA linking; no engine internal
patching; no mutation before Phase 4; no opaque errors; all process starts bind localhost;
reconcile state on every startup; cite upstream engine docs in comments/commits when non-obvious.

---

## 2. DATA MODEL — SQLite DDL (data-model.md)

SQLite WAL mode, one file `localhub.db`. Additive columns only; avoid destructive migrations.
**Required pragmas (verbatim top of schema):**
```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
```
Key design invariants: models vs instances are separate (a `model` is a catalog entry; an
`instance` is a running process; one model → 0..N instances). **Manifests are immutable** —
never mutate a manifest row; a re-download of a different revision is a new manifest. Metrics
are append-only, rolled up, pruned.

**Essential DDL (copied verbatim):**
```sql
CREATE TABLE models (
  id              TEXT PRIMARY KEY,          -- stable slug, e.g. "qwen2.5-coder-7b-q4_k_m"
  display_name    TEXT NOT NULL,
  source          TEXT NOT NULL,             -- ollama | huggingface | local_path | direct_url
  format          TEXT NOT NULL,             -- gguf | safetensors | pytorch | ollama
  quantization    TEXT,                      -- q4_k_m | q5_k_m | fp16 | bf16 | awq | gptq | fp8 | null
  repo            TEXT,                      -- HF repo id, if applicable
  revision        TEXT,                      -- HF commit sha (pinned), if applicable
  local_path      TEXT,                      -- absolute path on this host
  size_bytes      INTEGER,
  sha256          TEXT,                      -- of the primary artifact (or index)
  license         TEXT,
  recommended_engine TEXT,                   -- filled by compatibility engine
  compat_status   TEXT,                      -- runs | runs_degraded | wont_run | unknown
  manifest_id     TEXT REFERENCES manifests(id),
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE manifests (
  id              TEXT PRIMARY KEY,           -- uuid
  source          TEXT NOT NULL,
  repo            TEXT,
  revision        TEXT,                       -- exact commit sha
  files_json      TEXT NOT NULL,              -- [{path, sha256, size_bytes}]
  license         TEXT,
  format          TEXT,
  quantization    TEXT,
  security_warnings_json TEXT,                -- ["contains pickle .bin", "needs trust_remote_code"]
  downloaded_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE engines (
  name            TEXT PRIMARY KEY,           -- ollama | llamacpp | vllm
  installed       INTEGER NOT NULL DEFAULT 0,
  version         TEXT,
  base_url        TEXT,                       -- daemon engines (ollama)
  binary_path     TEXT,                       -- spawned engines (llama-server, vllm)
  running         INTEGER NOT NULL DEFAULT 0,
  last_seen       TEXT
);

CREATE TABLE instances (
  id              TEXT PRIMARY KEY,           -- uuid
  model_id        TEXT NOT NULL REFERENCES models(id),
  engine          TEXT NOT NULL REFERENCES engines(name),
  state           TEXT NOT NULL,              -- REQUESTED|STARTING|HEALTHY|DRAINING|STOPPED|FAILED
  pid             INTEGER,                    -- null for ollama
  port            INTEGER,                    -- null for ollama
  base_url        TEXT,
  vram_bytes_est  INTEGER,
  vram_bytes_obs  INTEGER,
  pinned          INTEGER NOT NULL DEFAULT 0, -- never auto-evict
  priority        INTEGER NOT NULL DEFAULT 0, -- higher = keep longer
  ttl_seconds     INTEGER,                    -- idle TTL; null = daemon default
  launch_flags_json TEXT,                     -- exact flags used (reproducibility)
  started_at      TEXT,
  last_used_at    TEXT,
  stopped_at      TEXT,
  fail_reason     TEXT
);

CREATE TABLE queue (
  id              TEXT PRIMARY KEY,           -- uuid
  model_id        TEXT NOT NULL REFERENCES models(id),
  engine          TEXT REFERENCES engines(name),
  priority        INTEGER NOT NULL DEFAULT 0,
  status          TEXT NOT NULL,              -- WAITING|ADMITTED|BLOCKED|DONE|REJECTED
  blocked_reason  TEXT,                       -- "insufficient VRAM, no evictable model"
  vram_bytes_est  INTEGER,
  enqueued_at     TEXT NOT NULL DEFAULT (datetime('now')),
  started_at      TEXT,
  finished_at     TEXT
);

CREATE TABLE metrics (
  ts              TEXT NOT NULL DEFAULT (datetime('now')),
  gpu_index       INTEGER NOT NULL,
  vram_used_bytes INTEGER,
  vram_total_bytes INTEGER,
  util_pct        REAL,
  temp_c          REAL,
  power_w         REAL
);
CREATE INDEX idx_metrics_ts ON metrics(ts);

CREATE TABLE inst_metrics (
  ts              TEXT NOT NULL DEFAULT (datetime('now')),
  instance_id     TEXT NOT NULL REFERENCES instances(id),
  tokens_per_s    REAL,
  ttft_ms         REAL,
  active_requests INTEGER,
  error_count     INTEGER
);
CREATE INDEX idx_inst_metrics_ts ON inst_metrics(ts);
```
**Retention:** metrics/inst_metrics raw 24–48h then roll up to 1-min averages + prune raw
(single timed `DELETE WHERE ts < ...`); prune queue `DONE`/`REJECTED` older than N days; keep
`STOPPED`/`FAILED` instance history. `launch_flags_json` is part of the reproducibility promise
(manifest + flags together fully reproduce a run).

---

## 3. API SURFACE (api-spec.md)

Two surfaces, kept separate: **Control API** (REST+SSE) and **Inference proxy** (Phase 6 only,
thin OpenAI passthrough). All endpoints bind `127.0.0.1` by default.

**Hardware**
- `GET /api/hardware` → `{cpu, ram, disk, gpus:[{index,name,vram_total,vram_free,driver,cuda,cc,temp,power,util}]}`
- `GET /api/hardware/live` → SSE stream of same, ~1 Hz

**Engines**
- `GET /api/engines` → `[{name, installed, version, running, base_url}]`
- `POST /api/engines/detect` → re-probe now

**Models (inventory)**
- `GET /api/models` → `[ModelSummary]` (normalized inventory)
- `GET /api/models/{id}` → `ModelDetail` (+manifest)
- `POST /api/models/import` `{source, path|repo|url, ref?}` → starts import job
- `DELETE /api/models/{id}` → remove from inventory (opt: delete files)

**Compatibility**
- `GET /api/models/{id}/compat?ctx=8192&batch=4` → Verdict
- `POST /api/compat/preview` `{repo|url, file?, ctx?}` → Verdict WITHOUT downloading (remote
  metadata read). **The money endpoint.**

**Instances (run/stop)**
- `GET /api/instances` → `[Instance]` (running + recent)
- `POST /api/instances` `{model_id, engine?, ctx?, priority?, pin?}` → Instance (async start)
- `GET /api/instances/{id}` → Instance (+state, +health)
- `DELETE /api/instances/{id}` → stop (drains active first unless `?force=true`)
- `PATCH /api/instances/{id}` `{pin?, priority?, ttl_seconds?}`
- `GET /api/instances/{id}/logs` → SSE log tail

**Queue**
- `GET /api/queue` → `[QueueEntry {status, blocked_reason, vram_est}]`
- `DELETE /api/queue/{id}` → cancel a waiting entry

**Metrics**
- `GET /api/metrics` → current snapshot (gpu + per-instance)
- `GET /api/metrics/live` → SSE stream
- `GET /metrics` → Prometheus exposition (text/plain) for Grafana users. Offer BOTH SSE and
  Prometheus.

**Inference proxy (Phase 6 only, thin)** — only after residency works:
- `GET /v1/models` → OpenAI-shaped list (union across engines)
- `POST /v1/chat/completions` → resolve model → admit (warm/evict/start) → proxy+stream
- `POST /v1/completions` → same
- `POST /v1/embeddings` → if embedding model served
Rules: streaming passes through UNTOUCHED (SSE chunks relayed as-is); on admission block
return **503 with JSON body carrying `blocked_reason`** — do not hang forever; do NOT
reimplement LiteLLM (point users at LiteLLM for gateway features) [ADR-002].

**Error contract (MANDATORY):** every error returns JSON
`{ "error": { "code", "message", "reason"? } }`. `reason` is the human-readable "why" —
mirrors the verdict/blocked-reason philosophy. **No opaque 500s.** Every user-facing
verdict/error MUST carry a human-readable `reason` (CLAUDE.md correctness bar).

**SSE contract:** server→client one-directional streams for metrics, logs, instance state.
Spec says prefer SSE over WebSocket for MVP (simpler, works through Tailscale Serve cleanly).
**NOTE — PLAN.md scope addition:** user requires WebSocket transport + JSON-RPC 2.0 + async
job API ALONGSIDE SSE. See §13 CONFLICTS/GAPS.

---

## 4. COMPATIBILITY ENGINE (compatibility-engine.md)

Answers per (model, hardware): runs? which engine? how much VRAM? full-GPU or CPU offload?
realistic ctx? exact flags? warning? Output = structured verdict; residency consumes the VRAM
estimate.

**GGUF path — do NOT reinvent.** Use `gguf-parser-go` (Apache-2.0,
github.com/gpustack/gguf-parser-go), same estimator GPUStack uses. Reads GGUF metadata via
HTTP range requests WITHOUT downloading whole model → verdict before a large download. Accounts
for ctx-size, gpu-layers, flash-attention, KV cache type, mmap, multi-device tensor-split;
emits JSON. Go: import as library. Parse **`VRAM.nonuma` (bytes)** as the estimate; compare
against **CURRENT free VRAM** from probe, not total. Do not write your own GGUF KV/weight
estimator [ADR-002].

**Safetensors / vLLM path — you must estimate yourself** (gguf-parser is GGUF-only). Three terms:

**(1) Weights** — prefer authoritative size in order:
1. `model.safetensors.index.json` → `metadata.total_size` (bytes, exact).
2. Sum of safetensors shard sizes from HF API file listing.
3. Fallback: `params × bytes_per_param` where bytes_per_param = 2 (fp16/bf16), 1 (fp8),
   ~0.5 (4-bit AWQ/GPTQ, add ~10% for scales/zeros overhead).

**(2) KV cache — GQA-aware (the common formula is WRONG).** KV scales with `n_kv_heads`, NOT
`n_attention_heads`. Using `hidden_size` over-estimates by the GQA ratio (often 4–8×). Exact math:
```
head_dim = hidden_size / n_attention_heads
kv_bytes = 2 (K and V)
         × n_layers
         × n_kv_heads          -- config: num_key_value_heads (NOT num_attention_heads)
         × head_dim
         × seq_len             -- context tokens you plan to serve
         × kv_dtype_bytes      -- 2 for fp16 KV, 1 for fp8 KV cache
         × batch               -- concurrent sequences (default modest, e.g. 4–8)
```
Read `num_key_value_heads`, `num_attention_heads`, `hidden_size`, `num_hidden_layers` from
`config.json` (fetchable from HF without weights). If `num_key_value_heads` absent → MHA →
`n_kv_heads = n_attention_heads`.

**(3) vLLM runtime overhead** (bigger than llama.cpp): CUDA context + activation buffers + CUDA
graph capture. MVP model: `overhead ≈ 1.5–2.0 GB` fixed, plus respect
`--gpu-memory-utilization` (default 0.9): vLLM tries to FILL that fraction with KV cache. Real
fit question: do weights fit inside `gpu_memory_utilization × VRAM` with enough left for KV to
serve `--max-model-len`? If weights alone exceed `gpu_memory_utilization × free_VRAM` →
`wont_run` without tensor-parallel/smaller quant/CPU offload (vLLM CPU offload is poor).

**Total (non-GGUF):**
```
est_vram = weights + kv_bytes(at target ctx & batch) + overhead
fits     = est_vram <= free_vram_now * safety_factor      -- safety_factor ~0.92
```

**Rule engine — rule-based, not ML** (auditable, users trust a stated reason).

**Hardware capability gates (checked FIRST):**
| Engine | Hard requirement |
|---|---|
| vLLM | NVIDIA CUDA compute capability **≥ 7.5**; Linux; Python 3.10–3.13; recent CUDA/ROCm **[RE-VERIFY]** |
| Ollama | broad; GPU or CPU; modern drivers |
| llama.cpp | broadest (CPU, CUDA, ROCm, Metal, Vulkan) |

If a model is safetensors-only and vLLM is gated out by CC → `runs_degraded` via llama.cpp
**iff a GGUF exists**, else `wont_run` on GPU (CPU offload possible but slow → say so).
Source note (compatibility-engine.md §revalidation 2026-07-06): vLLM install docs list CC ≥ 7.5;
do not loosen without checking exact vLLM version/build target.

**Engine recommendation logic:**
```
format == gguf         → llamacpp (or ollama if the model is in Ollama's registry)
format == safetensors  → vllm  (if hardware gate passes)
                       → else: suggest a GGUF equivalent for llamacpp, if known
awq / gptq / fp8       → vllm
ollama-registry ref    → ollama
```

**Context-length realism:** estimate at requested ctx AND at model max. If `runs` only at
reduced ctx → `runs_degraded` with the max ctx that fits. Surface "fits at 8K, not 32K on your 16 GB."

**Verdict contract (JSON):**
```json
{
  "model": "qwen2.5-coder-7b-q4_k_m.gguf",
  "hardware": { "gpu": "RTX 4060 Ti", "vram_total_mb": 16380, "vram_free_mb": 15200 },
  "status": "runs",                       // runs | runs_degraded | wont_run | unknown
  "engine_recommendation": "llamacpp",
  "estimated_vram_mb": 6200,
  "estimator": "gguf-parser-go@vX.Y.Z",   // or "localhub-safetensors" for the vLLM path
  "gpu_only": true,
  "cpu_offload_layers": 0,
  "max_context_that_fits": 16384,
  "requested_context": 8192,
  "run_flags": { "ctx_size": 8192, "n_gpu_layers": 99, "flash_attn": true },
  "warnings": [],
  "reason": "GGUF Q4_K_M weights ~4.4GB + KV ~1.6GB at 8K fits in 15.2GB free with headroom"
}
```
`reason` is NOT optional. **Deliberately NOT in v1:** no ML estimator; no exact-VRAM promise
(show estimate + margin, not false precision); no throughput/TPS promises (v2, label as rough).
**Correctness bar (CLAUDE.md):** a false `runs` that OOMs is a blocking correctness bug — prefer
conservative estimates + margins.

---

## 5. ENGINE ADAPTERS (engine-adapters.md)

**Ownership asymmetry (must be exposed, not papered over — residency depends on it):**
| Engine | Owns process | Start | Stop/free VRAM | Ports |
|---|---|---|---|---|
| Ollama | Ollama daemon (shared, not ours) | warm via API call | `keep_alive:0` (request unload) | one daemon/port |
| llama.cpp | **LocalHub** (spawn `llama-server`) | spawn process | SIGTERM the process | one port **per model** |
| vLLM | **LocalHub** (spawn `vllm serve`) | spawn process/container | SIGTERM (or sleep, advanced) | one port **per model** |
For llama.cpp/vLLM "unload" is deterministic/immediate (kill proc, OS reclaims VRAM). For
Ollama "unload" is a *request* to a daemon we don't control; happens on daemon schedule.

**Common contract (interface):**
```
detect()                  -> {installed, version, running, base_url?}
list_models()             -> [ModelRef]        # what this engine can serve
list_running()            -> [RunningInstance] # resident now (+vram, +expiry)
start(model, params)      -> Instance          # idempotent if already running
stop(instance)            -> ok                # best-effort free of VRAM
health(instance)          -> HEALTHY | STARTING | UNHEALTHY
supports_sleep()          -> bool              # true only for specially-launched vLLM
openai_base_url(instance) -> str               # where the thin proxy routes
```
`params` always includes `ctx`, and for owned engines `port` + memory knobs.

**Ollama adapter** (shared daemon; observe/nudge, never spawn per-model) **[RE-VERIFY endpoints]**:
- detect: `GET {base}/api/version` (default `http://127.0.0.1:11434`).
- list_models: `GET /api/tags` → installed models + sizes/digests.
- list_running: `GET /api/ps` → loaded models WITH VRAM size and `expires_at` — **source of
  truth for Ollama residency**.
- start (warm): minimal `POST /api/generate` (or `/api/chat`) with model + `keep_alive`. No
  explicit load endpoint; a request warms it.
- stop (unload): `POST /api/generate` `{"model":M,"keep_alive":0}` requests immediate unload.
  `keep_alive:-1` keeps resident indefinitely (pinned).
- health: model appears in `/api/ps` and responds.
- Coarse controls (env on daemon): `OLLAMA_MAX_LOADED_MODELS`, `OLLAMA_NUM_PARALLEL`,
  `OLLAMA_KEEP_ALIVE`, `OLLAMA_MODELS` (also `OLLAMA_MAX_QUEUE` per decisions.md revalidation).
- Honesty limits: new model must FULLY fit VRAM to co-load (else swaps); `keep_alive:0` is a
  request — poll `/api/ps` to confirm VRAM actually freed before assuming headroom.

**llama.cpp adapter (`llama-server`)** — LocalHub owns each process (llama-swap-shaped)
**[RE-VERIFY flags — PLAN.md warns local CUDA fork at `K:\projects\llm_models\llama.cpp-tq3\...`
may have fork-only flags; verify upstream vs fork]**:
- detect: `llama-server --version` (store binary in `engines.binary_path`).
- start: spawn bound to localhost on allocated port:
  `llama-server -m <gguf> --host 127.0.0.1 --port 8081 -c 8192 -ngl 99 --flash-attn --no-webui`
  Record PID, port, exact flags in `instances.launch_flags_json`.
- health: `GET http://127.0.0.1:PORT/health`.
- list_running: own `instances` table, reconciled by checking each PID + `/health`.
- stop: SIGTERM PID; on timeout SIGKILL; OS reclaims VRAM on exit.
- openai_base_url: `http://127.0.0.1:PORT/v1`.
- One model = one process = one port; LocalHub allocates ports from a pool.
- PLAN.md: existing `run-*.bat` profiles are direct input to flag templates (KV quant
  `-ctk q4_0 -ctv tq3_0`, `-fa on`, full-offload-or-fail, Ollama-unload preflight).

**vLLM adapter** — LocalHub owns process/container **[RE-VERIFY serve args + sleep gate]**:
- detect: `vllm --version` (or container image presence).
- start: spawn `vllm serve <model> --host 127.0.0.1 --port 8091 --gpu-memory-utilization 0.90
  --max-model-len 8192 --dtype auto` (`--tensor-parallel-size N` multi-GPU only).
- health: `GET http://127.0.0.1:PORT/health`.
- stop: SIGTERM; VRAM reclaimed on exit.
- openai_base_url: `http://127.0.0.1:PORT/v1`.
- **Cold start is slow** (weight load + CUDA graph capture, tens of seconds) — UI MUST show
  `STARTING` state with spinner + rough ETA. UX requirement, not nicety.
- **Sleep mode: advanced, opt-in, NOT default.** `/sleep`+`/wake_up` only exist when launched
  with `VLLM_SERVER_DEV_MODE=1 --enable-sleep-mode`. vLLM docs say these dev endpoints must NOT
  be exposed to users, only trusted networks. Level 1 offloads weights to CPU RAM (needs CPU
  RAM ≥ weights), discards KV; Level 2 discards weights too. `supports_sleep()` true only for
  instances LocalHub deliberately launched this way; keep endpoints localhost-bound, never
  proxy to clients; default residency = process stop.
- **GGUF on vLLM: do NOT route here.** vLLM GGUF support experimental/under-optimized, moved
  out of core (needs out-of-tree `vllm-gguf-plugin` per decisions.md revalidation). Adapter
  should refuse a GGUF start with a clear message.
- PLAN.md decision 3: build fully against vLLM's documented CLI/HTTP contract; continuous
  testing via stub/contract server on Windows; ONE controlled real E2E in Docker/WSL2 with a
  tiny (≤1B) model, then torn down.

**Adapter test checklist (per engine):** detect correct on clean+configured box; start→health
HEALTHY and model actually answers; stop actually frees VRAM (**verify via NVML delta, not just
process exit**); list_running matches reality after churn; reconciliation after restart
(owned re-attached/reaped, Ollama re-mirrored from `/api/ps`).

---

## 6. RESIDENCY (residency-scheduler.md, ADR-001)

**IS:** process-level admission control + eviction ("before loading X, is there room? if not,
unload an idle/expired/low-priority model first. Never kill an active generation. Never touch a
pinned model."). **IS NOT:** cross-engine VRAM scheduler / weighted-fair allocator / preemption.
NOT buildable on 3 independent engine processes [ADR-001]. **Do not let this creep back in — the
decision most likely to be wrongly "improved" back into fantasy.**

**ADR-003:** default recommendation is to **drive llama-swap** as the swap executor for owned
engines (design A) rather than hand-roll; build a custom minimal manager (design B) only if a
concrete limitation forces it. PLAN.md: llama-swap ships Windows Go binaries; resolve during
Phase 0.

**admit() algorithm (design B):**
```
admit(model_id, engine, priority):
    inst = running_instance(model_id, engine)
    if inst and inst.state == HEALTHY:
        touch(inst); return inst                       # already warm
    need = estimate_vram(model_id, engine)             # from compat engine
    free = gpu_free_vram()                              # NVML, live
    while free < need + SAFETY_MARGIN:
        victim = pick_victim(exclude_active=True)
        if victim is None:
            if priority >= HIGH: return reject("insufficient VRAM, no evictable model")
            else: enqueue(model_id, engine, priority, blocked_reason="waiting for VRAM"); return WAITING
        evict(victim)                                   # engine.stop() (or sleep, if enabled)
        free = gpu_free_vram()                          # RE-MEASURE; don't trust bookkeeping
    inst = engine.start(model_id, launch_params)
    wait_healthy(inst)
    return inst
```

**`pick_victim` ordering** (evict in this order, skipping ineligible):
1. Expired TTL (idle longer than `ttl_seconds`).
2. Idle + lowest priority (no active requests, lowest `priority`, then LRU `last_used_at`).
3. Largest reclaimable (only if still short; prefer freeing most VRAM).

**Never evict:** `pinned=1`; any instance with active in-flight requests (mark `DRAINING`,
wait, don't kill); the instance we're making room for.

**Re-measure rule (critical):** after each eviction read ACTUAL free VRAM from NVML, not a
running counter. Engines free asynchronously (esp. Ollama `keep_alive:0`), fragmentation
exists, other processes may use the GPU. Loading on bookkeeping that says "room" when there
isn't = the exact OOM this product prevents. (CLAUDE.md: `stop`/`evict` must verify VRAM freed
via NVML delta, not trust an API 200.)

**Never-kill-active-streams:** default = instance with active generations is `DRAINING`, not
killed; only after in-flight complete (or a drain timeout the user explicitly opted into) does
it stop. Core product promise. A config flag CAN allow force-eviction for a specific
high-priority request — OFF by default, with a loud warning.

**Concurrency across engines on one GPU:** no shared allocator (3 CUDA contexts). Only lever is
how much each is allowed resident, enforced by load/unload of whole models + each engine's own
knobs (`--gpu-memory-utilization`, `-ngl`, `OLLAMA_MAX_LOADED_MODELS`). Budget conservatively;
leave headroom; re-measure. Keep the sum of whole-model footprints under the card.

**Queue semantics (coarse, not fair):** `queue` table backs simple WAITING→ADMITTED flow.
Blocked low-priority waits until eviction frees room or a running model exits. `blocked_reason`
ALWAYS populated + shown in UI ("waiting: 3.2 GB short, no evictable model"). No starvation
guarantees in v1 (documented); add aging later if it bites. **NOTE:** PLAN.md decision 7
adds a required custom priority-queue algorithm with aging — see §13.

**Success = load-storm test:** rapidly request 6 models that can't all fit on 16 GB → no OOM,
no killed active streams, sensible evictions, clear blocked reasons, never a crash, never a
silent wrong answer.

---

## 7. SUPPLY CHAIN (model-supply-chain.md, ADR-004)

**Sources (MVP):** `huggingface` (repo snapshot at pinned revision), `gguf_url` (single .gguf
direct), `ollama` (`ollama pull`; Ollama owns artifact, we record ref+digest), `local_import`
(user points at existing file/dir).

**Core rule — pin the revision.** Never download "latest." Resolve HF ref to an exact commit
SHA and pin it. HF Hub API returns the commit for a ref; store SHA in both `models.revision`
and the manifest. `resolve(repo, ref="main") -> commit_sha`; `download(repo, revision=commit_sha)`.

**Manifest (immutable, one per download), stored in `manifests`:**
```json
{ "id":"b3f1…", "source":"huggingface", "repo":"Qwen/Qwen2.5-Coder-7B-Instruct",
  "revision":"e3a1c0f…", "format":"safetensors", "quantization":null, "license":"apache-2.0",
  "files":[ {"path":"model-00001-of-00004.safetensors","sha256":"…","size_bytes":4976000000},
            {"path":"config.json","sha256":"…","size_bytes":812} ],
  "security_warnings":[], "downloaded_at":"2026-07-06T10:00:00Z" }
```
Manifest + `instances.launch_flags_json` = full reproduction (what bytes, at what revision, with
what flags). Surface a "reproduce this" export.

**Hashing:** compute `sha256` per file during/after download → manifest. Where HF publishes
expected hashes/ETags, verify and flag mismatches. Primary artifact hash (or index file hash for
sharded) goes on `models` row for quick integrity checks.

**Security warnings (surface, don't silently block — ADR-004):** scan fileset, attach to manifest:
| Condition | Warning |
|---|---|
| `.bin`/`.pt`/`.pth` pickle weights, no safetensors | "Contains pickle-format weights. Pickle can execute code on load. Prefer a safetensors version." |
| requires `trust_remote_code` | "This model requires trust_remote_code — arbitrary code runs at load. Review the repo before enabling." |
| No license metadata | "No license detected. Check usage rights before deploying." |
| Gated/consent repo | "Gated model — access terms apply." |
Policy: **prefer** safetensors, **warn** loudly on pickle/`trust_remote_code`; do NOT hard-block
by default. User-set "strict mode" CAN hard-block — opt-in, not default.

**Disk accounting:** track `size_bytes` per model; show total used + free. Do NOT store both
safetensors and auto-converted GGUF (doubles disk; conversion is a landmine) [ADR-004]. Store
what user downloaded; conversion is separate/manual/advanced.

**Download UX:** show resolved commit SHA BEFORE downloading; show compatibility verdict BEFORE
committing to a large download (the point of gguf-parser remote read); resumable downloads;
verify hashes on completion; mark partial/failed clearly.

---

## 8. SECURITY (security.md)

Principle: security posture must NOT defeat the 5-minute install. Strong defaults, each layer
optional, degrades gracefully.

**Network exposure:** **bind `127.0.0.1` by default** (LocalHub can free/allocate GPU memory —
must not be on `0.0.0.0` out of box). Remote = Tailscale, recommended not required. Tailscale
**Serve** proxies localhost service to tailnet with ACLs + can pass identity headers (good
phone default). If Go, `tsnet` embeds Tailscale directly. LAN-only users can bind LAN IP behind
own firewall (supported, with warning no auth assumed). NEVER on public internet without reverse
proxy + auth.

**Identity/auth (MVP, layered):** (1) localhost trust — request on `127.0.0.1` is the machine
owner; (2) Tailscale identity — trust injected headers ONLY when LocalHub listens on localhost
and Tailscale is the proxy (never on arbitrary bind); (3) optional bearer token (single admin
token in config for LAN/headless). No OAuth/multi-tenant/RBAC in MVP.

**Dangerous endpoints — admin-only, localhost-bound, never client-facing:** vLLM `/sleep`,
`/wake_up`, `/collective_rpc`, `/reset_prefix_cache` (require `VLLM_SERVER_DEV_MODE=1`; never
exposed to users; if sleep enabled, keep localhost + behind admin auth; inference proxy never
forwards them). LocalHub's own instance stop/evict and `POST /api/instances` — admin-only.

**Model artifact safety:** prefer safetensors (safe format vs pickle which executes code on
load). Warn, don't hard-block by default; opt-in strict mode actually blocks
pickle/`trust_remote_code`. Warnings computed at download time, stored in manifest.

**Process isolation (proportional):** run control plane + engines as non-root by default;
support running engines in containers (don't require); gVisor only if/when a manual conversion
feature runs untrusted `trust_remote_code` — do not impose gVisor on whole product (auto-
conversion is cut from core, ADR-004).

**Threat model (ordered):** (1) malicious model artifact executing code at load → safetensors
preference + warnings + strict mode + non-root; (2) accidental public exposure of admin service
→ localhost-default + Tailscale + warnings; (3) resident model dev endpoints reachable →
localhost binding + not proxying. Out of scope: multi-tenant isolation, adversarial co-tenants,
supply-chain attacks on engines themselves.

**Tailscale Serve setup** (examples/tailscale-serve.md): default binds `http://127.0.0.1:39876`;
expose with `tailscale serve 39876`; open printed HTTPS tailnet URL on phone. Use Serve NOT
Funnel for private phone access. Phone check is a MANUAL gate item requiring the user's device
(PLAN.md).

---

## 9. TEST STRATEGY + OBSERVABILITY (15-test-strategy.md, 21-observability-spec.md)

**Test layers demanded:** (1) unit (parsers, estimators, manifests, queue/victim ordering,
state machine); (2) adapter contract tests — one suite per engine using fake HTTP servers +
process stubs; (3) hardware integration — real GPU, real engines, small models; (4) failure
tests — OOM prevention, port conflict, process crash, daemon restart, partial download, network
failure; (5) release acceptance — phase-gate matrix in
`project-management/acceptance-test-matrix.csv`.

**Critical correctness tests:**
- **Compatibility false-positive:** per validation model — record hardware snapshot; compute
  verdict; if `runs`, launch with generated flags, run a short prompt at requested ctx, record
  observed VRAM + result. **A false-positive `runs` that OOMs is a blocking bug.** False
  negatives acceptable early if conservative + explained.
- **Load-storm:** 6 models whose combined memory > GPU capacity; pin one; start one long
  streaming request; request rest with mixed priorities; assert: no active stream killed,
  pinned remains, idle/expired evicted first, blocked entries have reasons, no crash.
- **Manifest reproducibility:** export reproduce bundle; delete inventory row keep files;
  re-import from manifest; verify hashes; reproduce launch command.
- **Tailscale exposure:** binds `127.0.0.1` by default; same-machine browser works; phone over
  Serve works; LAN/public direct access fails unless explicitly configured; identity headers
  trusted only behind Serve + localhost bind.

**Test model set (min):** 5 GGUF (small/medium/high-ctx/Q4/Q8); 5 safetensors (fp16/bf16,
AWQ/GPTQ if available, GQA + MHA); 3 not-fit-on-16GB; 3 fit-only-at-reduced-ctx; 2 Ollama
registry; 2 local imports with incomplete metadata. Record exact repo/file/revision for every
test model. (PLAN.md constrains real loads: use existing ~60 GB GGUFs, tiny prompts, batched
sessions, negative tests via ctx-oversizing not giant downloads.)

**Observability — metrics to collect:**
- Hardware: GPU VRAM used/free/total, util, temp, power, driver + CUDA/ROCm version; CPU util;
  RAM used/free; disk used/free for model dir.
- Engine: installed/running/version, health endpoint status, base URL, models known, running.
- Instance: state, PID/port/base_url (owned), active requests, tokens/s, TTFT, errors,
  last-used ts, estimated + observed VRAM.
- Queue: waiting count, blocked reason, priority, vram estimate, age.

**Endpoints:** `GET /api/metrics`, `GET /api/metrics/live` (SSE), `GET /metrics` (Prometheus
text), `GET /api/instances/{id}/logs` (SSE).

**Logging rules:** log every launch command (secrets redacted); every compatibility verdict +
estimator version; every eviction decision with candidate list + reason; every rejected request
with blocked reason. **Do NOT log prompts or generated text by default.**

---

## 10. UI/UX (20-ui-ux-spec.md)

**MVP screens:** (1) Dashboard, (2) Models, (3) Compatibility Preview, (4) Downloads/Manifests,
(5) Running Instances, (6) Queue, (7) Settings/Tailscale.

**Dashboard must show:** GPU memory used/free/total; GPU util/temp/power; CPU/RAM/disk; running
engines + health; running model instances; queue length + blocked reasons; phone connection
status / Tailscale hint.

**Model table columns:** name, source, format, quantization, size, local path/repo, manifest
status, recommended engine, compatibility status, reason.

**Compatibility verdict UX — every verdict must display:** status (runs/runs_degraded/wont_run/
unknown), recommended engine, estimated VRAM, requested context, max context that fits,
generated run flags, warnings, human-readable reason. **Do NOT show a green `runs` badge without
the reason and margin.**

**Dangerous actions requiring explicit confirmation dialog:** stop, evict, delete files,
force-evict active generation, loading pickle/`trust_remote_code` models.

**Mobile (usable at 390px width):** GPU card first, running models second, queue third, actions
hidden behind menus, logs collapsed by default.

---

## 11. VERSION POLICY (22-engine-version-policy.md)

Version drift is a product risk. Each adapter records engine binary/container version; each
verdict records estimator version + engine recommendation basis; each launched instance records
exact launch flags. For vLLM prefer containerized/pinned Python env; for llama.cpp record binary
path + `llama-server --version`; for Ollama record daemon version + model digest.

**Support tiers:** Known-good (tested in current release gate) / Detected-untested (detected but
not in gate matrix — warn) / Unsupported (fails adapter contract or known incompatible).

**Release requirement — document known-good matrix:** OS, GPU, driver, CUDA/ROCm, Ollama
version, llama.cpp server version, vLLM version, gguf-parser-go version. (CLAUDE.md + PLAN.md:
engine claims are time-sensitive; each adapter task must re-verify current engine docs and cite
them; last revalidation 2026-07-06.)

---

## 12. TOP RISKS to design against (17-risk-register.md)

| Risk | Sev | Mitigation / close signal |
|---|---|---|
| Estimator says `runs` but OOMs | **Critical** | conservative margins; 20-model validation; false-positive block gate → zero false-positive runs in gate set |
| Cross-engine scheduler scope creep | **Critical** | ADR-001 + unwaivable gate → no fair-share/preemption code |
| Public exposure of admin controls | **Critical** | localhost default, Tailscale Serve, token → exposure test passes |
| vLLM version/CUDA churn breaks adapter | High | version pinning; Docker option; source revalidation → adapter matrix green for pinned |
| Model artifact RCE (pickle/trust_remote_code) | High | safetensors preference, warnings, strict mode, non-root → warning displayed + stored in manifest |
| Product duplicates GPUStack/Xinference | High | Phase 0 go/no-go; narrow to single-box bundle |
| Phase 6 becomes LiteLLM clone | High | thin proxy only; integration docs → no provider budgets/fallback/key mgmt |
| Ollama unload timing asynchronous | Medium | poll `/api/ps`; verify VRAM delta → stop gate verifies actual reclaim |
| Disk bloat from multiple formats | Medium | no auto conversion; disk accounting |
| Tailscale requirement hurts adoption | Medium | recommended not required; LAN mode with warning |

Unwaivable gates (PLAN.md, apply to every task): no false `runs`, no killed streams, manifests
always, localhost bind, no preemption scheduler, no auto-conversion, always a `reason`.

---

## 13. CONFLICTS / GAPS (for the orchestrator to resolve — NOT resolved here)

1. **Realtime transport: SSE-only (docs) vs SSE+WebSocket+JSON-RPC (PLAN.md).** api-spec.md
   §"Realtime: prefer SSE over WebSocket for MVP" explicitly says use SSE and "Use WebSocket
   only if you later need bidirectional low-latency control. Don't over-build here." PLAN.md
   decision 7 makes WebSocket transport + JSON-RPC 2.0 control surface + async submit-job API
   **required product-owner additions**, with architecture rule "one service core; REST, SSE,
   WebSocket, JSON-RPC are thin transports over it." The docs and PLAN directly disagree on
   whether WS/JSON-RPC exist in scope. The data model has NO table for async jobs (only
   `queue` for admission) — the async-inference job store is unspecified.

2. **Queue: coarse no-starvation (docs) vs custom priority algorithm with aging (PLAN.md).**
   residency-scheduler.md §"Queue semantics": "No starvation guarantees in v1; document this.
   If it bites, add simple aging later." PLAN.md decision 7(a) REQUIRES "a priority queue with
   a custom, documented algorithm ... priority classes + aging to prevent starvation +
   size/cost awareness." The `queue` DDL has only a single INTEGER `priority` and status enum —
   no priority-class, aging-timestamp, or cost columns; the schema must be extended additively
   (data-model.md allows additive columns). Algorithm is undocumented — must be authored.

3. **Hardware probe: NVML library (docs) vs `nvidia-smi` subprocess (PLAN.md).** architecture.md
   §Hardware probe and observability/roadmap say "NVIDIA-first via NVML (`go-nvml`/`pynvml`)."
   PLAN.md decision 5 overrides: NVML via cgo is painful on Windows and control plane must not
   link CUDA (ADR-005 spirit), so probe is an interface with `nvidia-smi` XML/CSV subprocess now
   (Windows), native NVML later (Linux). The residency "re-measure via NVML" rule and stop/evict
   "NVML delta" verification (residency-scheduler.md, engine-adapters.md, CLAUDE.md) must be read
   as "via the hardware-probe interface" — implementers must NOT link go-nvml on the Windows
   reference box. Terminology in specs ("NVML") is misleading for the actual Windows impl.

4. **vLLM presence: adapters/compat assume vLLM installable locally (docs) vs vLLM absent + no
   native Windows (PLAN.md).** engine-adapters.md and compatibility-engine.md spec vLLM
   detect/spawn/gates as if vLLM runs on the box. PLAN.md: vLLM has no native Windows support;
   only a single controlled Docker/WSL2 E2E with a ≤1B model is planned; continuous testing is
   against a stub/contract server. Gap: how the vLLM adapter's `detect()` and `start()` behave
   on Windows (subprocess vs docker exec vs container) is unspecified — the spec's bare
   `vllm serve ...` command assumes a local Linux binary that will not exist on the reference box.

5. **Linux paths / process signals vs Windows.** engine-adapters.md uses SIGTERM/SIGKILL and
   `/models/...` POSIX paths; residency/security assume non-root user and Unix process
   semantics. Reference box is Windows 10 (PLAN.md): process kill-trees, orphan reaping, port
   pooling, firewall-prompt-on-listen, and path handling differ. "SIGTERM then SIGKILL" must map
   to Windows process termination; "non-root user" (security.md) has no direct Windows analog.
   Implementers must keep the code portable (build tags/interfaces, per PLAN.md Approach) but the
   docs are written Linux-first.

6. **ADR-003 (llama-swap vs custom residency) unresolved in docs; PLAN says resolve in Phase 0.**
   residency-scheduler.md offers design A (drive llama-swap) as default and design B (custom
   manager) as fallback; ADR-003 status is "Proposed." task-breakdown LH-002 resolves it at G0.
   Implementers of Phase 5 need the resolution before coding — currently open. PLAN.md notes
   llama-swap ships Windows Go binaries. **Do not assume B; check the ADR-003 resolution first.**

7. **ADR-006 language: doc status "Proposed" vs PLAN "resolved to Go."** decisions.md ADR-006 and
   architecture.md present Go-vs-Python as an open choice ("decide explicitly ... don't
   relitigate"); PLAN.md decision 1 resolves it to Go and instructs recording the resolution
   (Proposed → Accepted) as part of Phase 1. Until decisions.md is updated, the two sources
   disagree on status. (Not a technical conflict — a bookkeeping gap.)

8. **Ollama `list_running` VRAM/`expires_at` field shape [RE-VERIFY].** engine-adapters.md asserts
   `/api/ps` returns VRAM size and `expires_at` per model; this is time-sensitive engine behavior
   (CLAUDE.md) and the exact field names/units are not pinned in the doc — must be re-verified
   against current Ollama API before the residency source-of-truth depends on it.

9. **`safety_factor` vs `SAFETY_MARGIN` — two different margin constructs, values loosely
   specified.** compatibility-engine.md uses a multiplicative `safety_factor ~0.92` (non-GGUF fit)
   and gguf-parser's own margin for GGUF; residency-scheduler.md uses an additive `SAFETY_MARGIN`
   in `admit()`; PLAN.md hardware safeguard uses `free VRAM − 1.5 GB` for dev tests. Three
   different margin conventions coexist with no single authoritative number — the estimator fit
   margin, the residency admission margin, and the dev-test safety cap must be reconciled into
   explicit constants.

10. **Async job result delivery store undefined.** PLAN.md decision 7(b) requires submit-job API
    returning a job id with poll + push result delivery. No table, endpoint, or SSE/WS event
    contract for async jobs exists in data-model.md or api-spec.md — entirely a gap to design.
