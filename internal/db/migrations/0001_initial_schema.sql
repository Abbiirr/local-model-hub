-- 0001_initial_schema.sql
-- Initial LocalHub schema. DDL copied VERBATIM from docs/data-model.md
-- (temp-research/spec-digest.md §2). Column names, order, types, and comments are kept
-- exactly as specified — do not "improve" them; additive columns only in later migrations.
--
-- NOTE on pragmas: PRAGMA journal_mode=WAL and foreign_keys=ON are connection-level, not
-- schema objects, and are set by the DB opener (internal/db/db.go) on every connection.
-- They are intentionally NOT run here because a migration runs inside a transaction and
-- `journal_mode=WAL` cannot change mode inside an active transaction.

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
