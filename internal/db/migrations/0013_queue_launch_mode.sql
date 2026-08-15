-- 0013_queue_launch_mode.sql
-- ADDITIVE migration (data-model.md: additive only; no destructive migrations). Every new column
-- is nullable or defaulted, so existing rows stay valid and older code simply ignores them.
--
-- WHY (a CONFIRMED LIVE DEFECT, observed 2026-08-06 — same class of bug as migration 0009):
-- 0009 taught the queue row to persist the KV pair / slots / speculative mode / CPU-offload
-- permission, but the llama.cpp SERVER-MODE fields — Embedding, Pooling, Reranking — were NOT
-- persisted. Consequence measured on this box:
--
--   * A start requested with {"model_id":"gguf-gpustack-bge-m3-gguf-bge-m3-fp16","embedding":true}
--     was queued (ram_blocked). When later admitted/replayed it launched with
--     `-ngl all -c 8192 --fit off -fa on -np 1 --no-webui` — NO `--embedding`. bge-m3 is
--     encoder-only, so llama-server crashed instantly ("health check failed: process exited
--     during startup"), then flapped in a retry loop.
--
-- The launch profile (compat.LaunchProfile) is the ONE description of a launch (M2.1/M2.2), so
-- EVERY behaviour-affecting field of it must survive enqueue → daemon restart → drain → start
-- byte-identically (M2.5 / TESTING.md#m2), exactly the property 0009 established for the sizing
-- fields. Embedding/Pooling/Reranking don't change the VRAM estimate, but they change which
-- ENDPOINTS the spawned process answers — dropping them on replay silently turns an
-- embedding/reranking request into a generation request, which is exactly the kind of dishonest
-- rewrite CLAUDE.md forbids.
--
--   req_embedding  1 => `--embedding` (embedding-only mode). NULL/0 => generation (unset).
--   req_pooling    `--pooling` value ("none"|"mean"|"cls"|"last"|"rank"). NULL/"" => unset (model
--                  default). Meaningful only with req_embedding.
--   req_reranking  1 => `--reranking`. Implies embedding + pooling=rank; compat.LaunchProfile.
--                  Normalize enforces that coupling on the profile that actually gets launched, so
--                  the raw persisted value here does not need to pre-couple it.
--
-- Columns mirror residency.ProfileSpec.{Embedding,Pooling,Reranking} exactly (residency/profile.go),
-- the same vocabulary as the 0009 columns and model_launch_defaults (0011).

ALTER TABLE queue ADD COLUMN req_embedding INTEGER NOT NULL DEFAULT 0; -- 1 => --embedding
ALTER TABLE queue ADD COLUMN req_pooling   TEXT;                       -- --pooling value; NULL/'' => unset
ALTER TABLE queue ADD COLUMN req_reranking INTEGER NOT NULL DEFAULT 0; -- 1 => --reranking
