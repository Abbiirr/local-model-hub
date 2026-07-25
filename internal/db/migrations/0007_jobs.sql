-- 0007_jobs.sql
-- Additive migration (data-model.md: additive only). P6.2 async inference jobs (PLAN.md
-- decision 7b + conflict-resolution 10): submit an inference request, get a job id back
-- immediately, poll or receive the result over WebSocket, cancel it, and survive a daemon
-- restart (research/lmwrapper-reuse-notes.md restart lesson: queued jobs MUST persist — the
-- lmwrapper failure was that they did not). This is a NEW table; nothing existing is touched.
--
-- A job is the durable record of an async inference request routed through the SAME residency
-- admission + P5.0 ordering as everything else. state machine:
--   queued    -> accepted, waiting for admission/VRAM (durable; survives restart)
--   running   -> admitted, relaying to the engine (an instance is HEALTHY and serving it)
--   succeeded -> engine returned a complete result (result_json holds the OpenAI response)
--   failed    -> engine/admission error, or 'daemon restarted mid-inference' on reconcile
--   cancelled -> user cancelled (mid-queue removal or mid-inference server-side abort)
-- result_json is the full non-streaming OpenAI response body on success (nullable otherwise).
-- error/reason carry the human-readable why on failure (mandatory-reason gate).

CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,               -- uuid
  model_id      TEXT NOT NULL,                  -- the requested model slug (resolved like the proxy)
  engine        TEXT,                           -- resolved engine (nullable until admission picks it)
  request_json  TEXT NOT NULL,                  -- the OpenAI request body as submitted (verbatim)
  priority      INTEGER NOT NULL DEFAULT 1,     -- raw priority (0 interactive|1 normal|2 background)
  state         TEXT NOT NULL DEFAULT 'queued', -- queued|running|succeeded|failed|cancelled
  result_json   TEXT,                           -- OpenAI response body on success (nullable)
  error         TEXT,                           -- machine-ish error class on failure (nullable)
  reason        TEXT,                           -- human-readable why (failure/cancel) (nullable)
  queue_id      TEXT,                           -- residency queue row id while queued (nullable)
  instance_id   TEXT,                           -- serving instance while running (nullable)
  endpoint      TEXT NOT NULL DEFAULT 'chat',   -- 'chat' | 'completions' (which OpenAI path to relay to)
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  started_at    TEXT,                           -- when it went running
  finished_at   TEXT                            -- when it reached a terminal state
);
CREATE INDEX idx_jobs_state_created ON jobs(state, created_at);  -- worker scan: queued oldest-first
