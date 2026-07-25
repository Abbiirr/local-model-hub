-- 0002_model_presence_files.sql
-- Additive migration (data-model.md: additive columns only; no destructive migrations).
-- Approved by orchestrator sign-off (P1 wiring task, 2026-07-06). Three new nullable/
-- defaulted columns on `models`, all backward-compatible with 0001:
--
--   presence       TEXT  'present' | 'missing'  (DEFAULT 'present')
--     Reconcile marks rows whose artifact vanished from a *scanned* source as 'missing'
--     WITHOUT deleting them. It never touches rows from an un-scanned source (e.g. Ollama
--     rows when the daemon was unreachable). Existing rows default to 'present' so a plain
--     0001->0002 upgrade leaves every catalog row present. This replaces the interim
--     compat_status='missing' overload (which corrupted the compat vocabulary
--     runs|runs_degraded|wont_run|unknown) — see temp-research/wiring-p14.md §5B.
--
--   missing_since  TEXT  (nullable)
--     RFC3339/datetime timestamp set when a row first transitions to 'missing'; cleared
--     (set NULL) when the artifact reappears in a later scan. NULL for present rows.
--
--   files_json     TEXT  (nullable)
--     Captures scanner-discovered companions (mmproj vision projectors) and multi-part
--     shard lists that have no first-class column in 0001. Shape:
--       {"companions":[{"kind","path","size_bytes"}],
--        "parts":[path,...],
--        "architectures":[...], "model_type":"..."}
--     These are load-bearing for the llama.cpp launch flags in Phase 4 (--mmproj, shard-0
--     path) and for the compat engine (architectures/model_type from a safetensors
--     config.json), which had no home in the 0001 DDL (wiring-p14.md §7 gaps 2 & 3).
--     Persisting HF architectures/model_type here fulfils orchestrator sign-off item 4
--     without a separate column. NULL when a model has no companions/parts/arch metadata.
--
-- SQLite ALTER TABLE ADD COLUMN is a fast metadata-only change and is transaction-safe;
-- the migration runner (internal/db/migrate.go) wraps this file in its own transaction.

ALTER TABLE models ADD COLUMN presence TEXT NOT NULL DEFAULT 'present';
ALTER TABLE models ADD COLUMN missing_since TEXT;
ALTER TABLE models ADD COLUMN files_json TEXT;
