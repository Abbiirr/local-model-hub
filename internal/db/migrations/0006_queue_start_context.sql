-- 0006_queue_start_context.sql
-- Additive migration (data-model.md: additive only; no destructive migrations). Phase 5 fix 1
-- (the production queue-drain loop) needs the queue row to persist the FULL start context of a
-- queued admission request so the drain loop can re-Admit and start it later WITHOUT the original
-- HTTP caller. Before this, only model_id/engine/priority_class/footprint survived enqueue; the
-- context length, batch, and pin flag were dropped — so an auto-started queued request would have
-- lost the ctx/pin the user asked for. Every new column is nullable/defaulted so existing rows
-- stay valid.
--
--   req_ctx    The requested context length (-c) for the model load. Restored by the drain loop
--              so a queued request starts at the same ctx the user requested (not the daemon
--              default). NULL → the start path applies the compat default context.
--   req_batch  The requested batch size. NULL → start-path default.
--   req_pin    Whether the started instance should be pinned (never auto-evict). 0 = not pinned.
--              A pinned request that queued must come back pinned when it drains.
--
-- The raw `priority` INTEGER (0001) already carries the user's priority verbatim and priority_class
-- (0005) carries the derived class, so priority survives; only ctx/batch/pin were missing.

ALTER TABLE queue ADD COLUMN req_ctx   INTEGER;                    -- requested context length (-c)
ALTER TABLE queue ADD COLUMN req_batch INTEGER;                    -- requested batch size
ALTER TABLE queue ADD COLUMN req_pin   INTEGER NOT NULL DEFAULT 0; -- 1 = pin the started instance
