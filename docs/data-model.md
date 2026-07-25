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
