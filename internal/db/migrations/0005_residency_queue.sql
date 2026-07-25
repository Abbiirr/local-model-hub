-- 0005_residency_queue.sql
-- Additive migration (data-model.md: additive only; no destructive migrations). Phase 5 (P5.0
-- custom priority queue + P5.1 admission control) needs a few extra columns on the existing
-- `queue` table (0001 DDL) to persist the custom-algorithm state, plus an index for the drain
-- scan. No existing table/column is touched; every new column is nullable/defaulted so existing
-- rows stay valid (there are none — no Go code wrote the queue table before Phase 5).
--
-- WHY these columns (PLAN.md spec-conflict resolution 2: the single INTEGER `priority` + status
-- enum in the 0001 DDL cannot express priority CLASSES, aging, or coalescing — the custom
-- algorithm in docs/residency-priority-queue.md; the schema must be extended additively):
--
--   priority_class   The coarse tier the ordering algorithm uses: 0 interactive / 1 normal /
--                    2 background (lmwrapper-compatible). Distinct from the raw `priority` INTEGER
--                    (which 0001 keeps as "higher = keep longer" for instances); a queued request
--                    is ordered by CLASS + aging + size/cost tie-break. Defaults 1 (normal).
--   seq              The monotonic arrival sequence assigned at enqueue — the FIFO / total-order
--                    key that makes ordering deterministic (no equal-timestamp ties from the
--                    second-resolution enqueued_at). Ascending = older.
--   footprint_mib    The admission footprint estimate (per-engine model) used for this request —
--                    stored so a restart / the drain loop can re-run the fit test without a
--                    re-estimate, and so the size/cost tie-break agrees with admission. Mirrors
--                    the existing vram_bytes_est but in MiB and always the admission number.
--   coalesce_of      When a request was admitted as a same-model coalescing pick, the model_id it
--                    was coalesced behind (provenance / observability). NULL otherwise.
--   updated_at       Last time the row's status/blocked_reason changed (drain progress). Powers
--                    the queue panel's "age" and the periodic drain sweep ordering.
--
-- The `status` enum (WAITING|ADMITTED|BLOCKED|DONE|REJECTED) and `blocked_reason` already exist in
-- 0001 and are reused verbatim: a queued request is WAITING (blocked_reason names the numbers);
-- ADMITTED when admission later succeeds; DONE/REJECTED terminal. The in-memory Queue
-- (internal/residency) is the ordering authority; these rows are the DURABLE mirror for the API,
-- the UI, and restart survival (the reconciler re-seeds the in-memory queue from WAITING rows).

ALTER TABLE queue ADD COLUMN priority_class INTEGER NOT NULL DEFAULT 1; -- 0 interactive|1 normal|2 background
ALTER TABLE queue ADD COLUMN seq            INTEGER;                    -- monotonic arrival sequence (FIFO key)
ALTER TABLE queue ADD COLUMN footprint_mib  INTEGER;                    -- admission footprint estimate (MiB)
ALTER TABLE queue ADD COLUMN coalesce_of    TEXT;                       -- model_id this was coalesced behind (nullable)
ALTER TABLE queue ADD COLUMN updated_at     TEXT NOT NULL DEFAULT (datetime('now'));

-- Drain scan reads WAITING rows in arrival order; index status+seq for a cheap ordered sweep.
CREATE INDEX IF NOT EXISTS idx_queue_status_seq ON queue(status, seq);
