# Data Model

SQLite in WAL mode. One file, `localhub.db`. No ORM required; the schema is small.
Everything below is MVP; add columns additively, avoid destructive migrations.

## Design notes

- **Models vs instances are separate.** A `model` is a catalog entry (an artifact on
  disk / a pullable ref). An `instance` is a *running* engine process serving that model.
  One model → zero or more instances over time.
- **Manifests are immutable.** Never mutate a manifest row after write; a re-download of a
  different revision is a new manifest.
- **Metrics are append-only, rolled up, and pruned.** Do not keep per-second rows forever.

## Schema (DDL)

```sql
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- Physical model artifacts known to LocalHub -------------------------------
CREATE TABLE models (
  id              TEXT PRIMARY KEY,          -- stable slug, e.g. "qwen2.5-coder-7b-q4_k_m"
  display_name    TEXT NOT NULL,
  source          TEXT NOT NULL,             -- ollama | huggingface | local_path | direct_url
  format          TEXT NOT NULL,             -- gguf | safetensors | pytorch | ollama
  quantization    TEXT,                      -- q4_k_m | q5_k_m | fp16 | bf16 | awq | gptq | fp8 | null
  repo            TEXT,                       -- HF repo id, if applicable
  revision        TEXT,                      -- HF commit sha (pinned), if applicable
  local_path      TEXT,                      -- absolute path on this host
  size_bytes      INTEGER,
  sha256          TEXT,                       -- of the primary artifact (or index)
  license         TEXT,
  recommended_engine TEXT,                    -- filled by compatibility engine
  compat_status   TEXT,                       -- runs | runs_degraded | wont_run | unknown
  manifest_id     TEXT REFERENCES manifests(id),
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Immutable provenance record produced at download time --------------------
CREATE TABLE manifests (
  id              TEXT PRIMARY KEY,           -- uuid
  source          TEXT NOT NULL,
  repo            TEXT,
  revision        TEXT,                        -- exact commit sha
  files_json      TEXT NOT NULL,               -- [{path, sha256, size_bytes}]
  license         TEXT,
  format          TEXT,
  quantization    TEXT,
  security_warnings_json TEXT,                  -- ["contains pickle .bin", "needs trust_remote_code"]
  downloaded_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Detected inference engines on this host ----------------------------------
CREATE TABLE engines (
  name            TEXT PRIMARY KEY,           -- ollama | llamacpp | vllm
  installed       INTEGER NOT NULL DEFAULT 0, -- bool
  version         TEXT,
  base_url        TEXT,                        -- for daemon engines (ollama)
  binary_path     TEXT,                        -- for spawned engines (llama-server, vllm)
  running         INTEGER NOT NULL DEFAULT 0,
  last_seen       TEXT
);

-- Running (or recently-running) engine processes --------------------------
CREATE TABLE instances (
  id              TEXT PRIMARY KEY,           -- uuid
  model_id        TEXT NOT NULL REFERENCES models(id),
  engine          TEXT NOT NULL REFERENCES engines(name),
  state           TEXT NOT NULL,               -- REQUESTED|STARTING|HEALTHY|DRAINING|STOPPED|FAILED
  pid             INTEGER,                     -- null for ollama (shared daemon)
  port            INTEGER,                     -- null for ollama
  base_url        TEXT,                        -- where to route requests
  vram_bytes_est  INTEGER,                     -- estimate at launch
  vram_bytes_obs  INTEGER,                     -- observed, if attributable
  pinned          INTEGER NOT NULL DEFAULT 0,  -- never auto-evict
  priority        INTEGER NOT NULL DEFAULT 0,  -- higher = keep longer
  ttl_seconds     INTEGER,                     -- idle TTL; null = daemon default
  launch_flags_json TEXT,                      -- exact flags used (reproducibility)
  started_at      TEXT,
  last_used_at    TEXT,
  stopped_at      TEXT,
  fail_reason     TEXT
);

-- Coarse admission queue ---------------------------------------------------
CREATE TABLE queue (
  id              TEXT PRIMARY KEY,           -- uuid
  model_id        TEXT NOT NULL REFERENCES models(id),
  engine          TEXT REFERENCES engines(name),
  priority        INTEGER NOT NULL DEFAULT 0,
  status          TEXT NOT NULL,               -- WAITING|ADMITTED|BLOCKED|DONE|REJECTED
  blocked_reason  TEXT,                        -- "insufficient VRAM, no evictable model"
  vram_bytes_est  INTEGER,
  enqueued_at     TEXT NOT NULL DEFAULT (datetime('now')),
  started_at      TEXT,
  finished_at     TEXT
);

-- Time-series metrics (append-only, pruned) --------------------------------
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

-- Per-instance perf samples ------------------------------------------------
CREATE TABLE inst_metrics (
  ts              TEXT NOT NULL DEFAULT (datetime('now')),
  instance_id     TEXT NOT NULL REFERENCES instances(id),
  tokens_per_s    REAL,
  ttft_ms         REAL,
  active_requests INTEGER,
  error_count     INTEGER
);
CREATE INDEX idx_inst_metrics_ts ON inst_metrics(ts);

-- Async inference jobs (P6.2) ----------------------------------------------
-- Durable record of an async inference request routed through the SAME residency admission
-- as everything else (migration 0007_jobs.sql). A job is submitted, gets an id back
-- immediately, and survives a daemon restart. State machine:
--   queued -> running -> succeeded | failed | cancelled
-- request_json is the OpenAI body verbatim (so a restart re-runs the exact request);
-- result_json is the final non-streaming OpenAI response on success; error/reason carry the
-- human-readable why on failure/cancel (mandatory-reason gate).
CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,               -- uuid
  model_id      TEXT NOT NULL,                  -- requested model slug (resolved like the proxy)
  engine        TEXT,                           -- resolved engine (nullable until admission picks it)
  request_json  TEXT NOT NULL,                  -- OpenAI request body as submitted (verbatim)
  priority      INTEGER NOT NULL DEFAULT 1,     -- 0 interactive | 1 normal | 2 background
  state         TEXT NOT NULL DEFAULT 'queued', -- queued|running|succeeded|failed|cancelled
  result_json   TEXT,                           -- OpenAI response body on success (nullable)
  error         TEXT,                           -- machine-ish error class on failure (nullable)
  reason        TEXT,                           -- human-readable why (failure/cancel) (nullable)
  queue_id      TEXT,                           -- residency queue row id while queued (nullable)
  instance_id   TEXT,                           -- serving instance while running (nullable)
  endpoint      TEXT NOT NULL DEFAULT 'chat',   -- 'chat' | 'completions' (which OpenAI path)
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  started_at    TEXT,
  finished_at   TEXT
);
CREATE INDEX idx_jobs_state_created ON jobs(state, created_at);
```

## Retention

- `metrics` / `inst_metrics`: keep raw for 24–48h, then roll up to 1-min averages, prune
  raw. A single `DELETE ... WHERE ts < ...` on a timer is enough at this scale.
- `queue`: prune `DONE`/`REJECTED` older than N days.
- `instances`: keep `STOPPED`/`FAILED` history (small, useful for debugging launches).
- `jobs`: keep terminal (`succeeded`/`failed`/`cancelled`) rows for a bounded window, then
  prune (results can be large — prune `result_json`-bearing rows first).

## Why `launch_flags_json` matters

Storing the *exact* flags used to start an instance (ngl, ctx-size, gpu-memory-util,
max-model-len, dtype, tensor-parallel-size) is part of the reproducibility promise: the
UI can show "run command preview" and the manifest + flags together fully reproduce a
working run. This is cheap and is one of the things competitors don't surface.

## `observed_peaks.kv_type` — the calibration key (added 2026-07-25)

> **This doc's §Schema (DDL) predates migrations 0003-0011.** `observed_peaks` (0003),
> `instance_lifecycle` (0004), the residency `queue` (0005/0006/**0009**), `jobs` (0007), `nodes`
> (0008), the instance health/reclaim columns (**0010**), and `model_launch_defaults` (**0011**) are
> additive tables/columns not listed above; the migration files are authoritative. Only fields whose
> *semantics* changed are documented here.

### `queue.req_*` — the persisted launch profile (0009, added 2026-07-25)

0006 taught a queue row to persist `req_ctx`/`req_batch`/`req_pin` so the drain loop could restart a
queued request without the original HTTP caller. It did **not** persist the KV description, the
CPU-offload permission, or the speculative mode — a **reproduced live defect**: a start queued with
`kv_quant=true` replayed with an **f16** cache, which for the Qwen3.6-27B prices at ~15602 MiB against
~14168 MiB usable, so the row sat `WAITING` forever. A request that would have fit at q4_0 (~14107 MiB)
could never drain; the queue silently rewrote it into an impossible one.

0009 adds `req_kv_type_k`, `req_kv_type_v`, `req_slots`, `req_spec_mode`, `req_spec_draft_max`,
`req_allow_cpu_offload` (all nullable or defaulted; every read COALESCEd, so rows written before the
migration load as "no advanced knobs"). **The membership rule:** a field belongs here iff it changes the
emitted llama-server flags **or** the VRAM estimate. Anything that can and is missing is a replay bug.

The legacy `kv_quant` bool deliberately gets **no column**. Storing both a bool and a pair invites them
to disagree after a restart, so the bool is resolved to the (K,V) pair **once** at enqueue
(`core.startProfile` precedence) and only the pair is persisted.

### `instances.last_health_at` / `health_failures` / `reclaim_verified` (0010, added 2026-07-25)

`last_health_at` (UTC text, same format as every other time column) is the last **successful** health
probe: a bare state of `HEALTHY` says nothing about staleness. `health_failures` counts **consecutive**
failed probes, reset to 0 on any success, and is **persisted** rather than kept only in memory so a
daemon restart cannot silently forgive a long-failing instance.

`reclaim_verified` is deliberately **three-valued**: `1` = free VRAM measurably rose by at least
(footprint − slack), proven by an NVML delta; `0` = the process was signalled/killed but reclamation was
**not** proven (timed out, or the baseline probe failed so no delta is meaningful); `NULL` = no stop
attempted. "Not attempted" must never read as "failed". The stop `reason` string was always truthful,
but `state='STOPPED'` plus `vram_freed_mib` alone let a reclaim timeout look like a clean stop to a
programmatic consumer.

No new instance **state** was added: the machine stays
`REQUESTED→STARTING→HEALTHY→DRAINING→STOPPED/FAILED`, and these columns add observability to existing
states rather than threading an `UNHEALTHY` state through every transport.

### `model_launch_defaults` (0011, added 2026-07-25)

One saved, proven launch profile per model (`model_id` PRIMARY KEY, newest write wins — the same shape
as `observed_peaks`), with the same column vocabulary as `queue.req_*` plus provenance
(`engine_version`, `engine_commit`, `binary_path`, `artifact_sha256`, `validated_note`).

Two constraints are structural, not incidental: the start path **never reads it** (it exists to pre-fill
the UI form, which the operator confirms — TODO M6.1), and it is **provenance-bound**, because a profile
validated against one engine build is not evidence about another (TODO M6.2). An `artifact_sha256` of
`NULL` means "provenance unverified", never "matches".

`observed_peaks` records a measured VRAM peak so a later verdict can prefer a real number over an
estimate (the lmwrapper measured-VRAM lesson). Columns (`0003_observed_peaks.sql`):

| Column | Type | Meaning |
|---|---|---|
| `model_key` | TEXT | catalog model id the measurement belongs to |
| `peak_vram_mib` | INTEGER | measured peak footprint |
| `ctx` | INTEGER | context length in force at measurement |
| `kv_type` | TEXT | **KV cache identity in force at measurement — see below** |
| `gpu`, `driver` | TEXT | provenance; a peak measured on other hardware is not portable |

**Lookups are keyed by `(model_key, ctx, kv_type)`.** Since `-ctk` and `-ctv` became independent,
one column must represent a *pair*, so `gguf.CanonicalKVKey(k, v)` renders it:

- **symmetric** pair → the bare type: `f16`, `q8_0`, `q4_0` (also `tq3_0` on the CUDA fork).
  Byte-identical to what pre-independent-K/V code wrote, so **every existing row still matches**
  and no migration is needed — the column stays plain `TEXT`.
- **asymmetric** pair → `k/v`, e.g. `q8_0/q4_0` (the validated Qwen3.6-27B MTP profile).
- empty sides resolve to `f16`, matching the estimator default.

An asymmetric profile therefore has **no calibration data until a peak is measured for that exact
pair** and correctly falls back to the estimate. That miss is deliberate: a peak measured with a
q4_0 value cache must never be reused to justify a q8_0 one, which would under-price the KV and
risk a false `runs`.

**Writers must use the same rendering as readers.** `internal/core` records peaks via
`compat.KVTypeName(plan.KVType)`; the estimator's lookup path builds the key from
`gguf.Options.KVCacheType`, which `internal/compat/gguf_path.go` populates with
`CanonicalKVKey(kvK, kvV)`. Leaving that legacy field empty makes the key `""`, so **every**
recorded measurement misses and verdicts silently degrade from `exact` to `fallback` confidence —
a regression that shipped once and is now pinned by `TestVerdictUsesObservedPeak` /
`TestVerdictTightensAfterRecording`. See KNOWN-LIMITATIONS #20 and
docs/compatibility-engine.md §2.
