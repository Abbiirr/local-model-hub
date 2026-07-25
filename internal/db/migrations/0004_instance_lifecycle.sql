-- 0004_instance_lifecycle.sql
-- Additive migration (data-model.md: additive only; no destructive migrations). Phase 4
-- (P4.1 instance state machine + reconciliation, P4.2 llama.cpp adapter) needs a few extra
-- columns on `instances` that the 0001 DDL did not have, plus a small table to track state
-- transitions with timestamps + reasons, and a port-pool ledger. No existing table/column is
-- touched; every new column is nullable/defaulted so existing rows stay valid.
--
-- WHY these columns (P4.1/P4.2 gates, spec-digest §5/§6, TESTING.md#p41 #p42):
--   binary_path       The exact llama-server binary that launched this instance. Recorded at
--                     spawn so reconciliation can verify identity (port + model path + binary)
--                     and the reproduce export names the engine binary (docs/22 version policy).
--   model_path        The on-disk GGUF the instance serves. Reconciliation compares the recorded
--                     model_path against the live process to detect a reused-PID mismatch.
--   vram_freed_mib    The MEASURED NVML VRAM delta reclaimed on stop (loaded footprint freed).
--                     A stop is only "verified" when this rises by >= footprint-500MiB (gate:
--                     an HTTP-200-based stop claim is FORBIDDEN). NULL until a verified stop.
--   ctx               The context length the instance was launched at (tokens). Part of the
--                     launch identity and the observed-peak provenance.
--   reason            The latest human-readable state reason (mirrors fail_reason but for any
--                     state, e.g. "orphan reaped at reconcile", "process gone at reconcile").
--   created_at        When the instance row was first created (REQUESTED). started_at in 0001 is
--                     when it went HEALTHY; created_at anchors the full lifecycle timeline.
--
-- instance_transitions is the persisted, append-only audit of every legal state transition
-- (REQUESTED->STARTING->HEALTHY->DRAINING->STOPPED|FAILED) with a timestamp and reason. The
-- state machine writes one row per transition; the instances.state column always holds the
-- current state (denormalized for fast reads), and the transitions table holds the history the
-- P4.1 gate requires ("state machine transitions persisted").
--
-- port_pool is the persisted allocation ledger for the llama.cpp port range (config
-- engines.llamacpp.port_range, default 40100-40200). A port is INSERTed when allocated to an
-- instance and DELETEd when freed on stop; allocation skips ports that fail a bind-test. This
-- survives a daemon restart so reconciliation knows which ports were ours.

ALTER TABLE instances ADD COLUMN binary_path    TEXT;    -- llama-server binary that launched it
ALTER TABLE instances ADD COLUMN model_path     TEXT;    -- on-disk GGUF served (identity check)
ALTER TABLE instances ADD COLUMN vram_freed_mib INTEGER; -- measured NVML delta freed on stop (gate)
ALTER TABLE instances ADD COLUMN ctx            INTEGER; -- launch context length (tokens)
ALTER TABLE instances ADD COLUMN reason         TEXT;    -- latest human-readable state reason
ALTER TABLE instances ADD COLUMN created_at     TEXT NOT NULL DEFAULT (datetime('now'));

CREATE TABLE instance_transitions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id  TEXT NOT NULL REFERENCES instances(id),
  from_state   TEXT,                       -- null for the very first (REQUESTED) row
  to_state     TEXT NOT NULL,              -- REQUESTED|STARTING|HEALTHY|DRAINING|STOPPED|FAILED
  reason       TEXT NOT NULL,              -- human-readable why (mandatory)
  at           TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_instance_transitions_iid ON instance_transitions(instance_id);

CREATE TABLE port_pool (
  port        INTEGER PRIMARY KEY,        -- the allocated TCP port
  instance_id TEXT NOT NULL,              -- who holds it (freed = row deleted on stop)
  allocated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Seed the fixed engine enum rows. instances.engine REFERENCES engines(name), so a spawned
-- instance (engine='llamacpp'|'vllm') needs its engine row to exist or the FK fails. The
-- engines table is otherwise only read by detection (P1.3, read-only) and never populated, so
-- we seed the three known engines here. `installed`/`running` are refreshed by detection at
-- runtime; these seed rows just satisfy the FK. INSERT OR IGNORE keeps it idempotent if a row
-- already exists.
INSERT OR IGNORE INTO engines (name, installed, running) VALUES
  ('ollama',   0, 0),
  ('llamacpp', 0, 0),
  ('vllm',     0, 0);
