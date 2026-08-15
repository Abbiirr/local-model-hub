-- 0009_queue_launch_profile.sql
-- ADDITIVE migration (data-model.md: additive only; no destructive migrations). Every new column
-- is nullable or defaulted, so existing rows stay valid and older code simply ignores them.
--
-- WHY (a CONFIRMED LIVE DEFECT, observed 2026-07-25 — KNOWN-LIMITATIONS #21):
-- 0006 taught the queue row to persist req_ctx / req_batch / req_pin so the drain loop could
-- re-start a queued request faithfully. But the KV cache description, the CPU-offload permission,
-- and the speculative mode were NOT persisted. Consequences measured on this box:
--
--   * A start queued with kv_quant=true replayed with an f16 KV cache. For the Qwen3.6-27B MTP
--     model that prices at ~15602 MiB against ~14168 MiB usable, so the entry sat WAITING FOREVER
--     with "footprint 15602 + 1024 margin" — a request that WOULD have fit at q4_0 (~14107 MiB)
--     could never drain. The queue silently turned the user's request into an impossible one.
--   * allow_cpu_offload was dropped, so a MoE queued WITH expert offload replayed without -ncmoe
--     and was sized against the full (larger) footprint.
--   * speculative was dropped, so a queued ngram-cache / draft-mtp start replayed with no
--     speculative decoding at all.
--
-- The launch profile (compat.LaunchProfile) is the ONE description of a launch (M2.1/M2.2), so
-- EVERY behaviour-affecting field of it must survive enqueue → daemon restart → drain → start
-- byte-identically (M2.5 / TESTING.md#m2). Anything that changes the emitted flags OR the VRAM
-- estimate belongs here.
--
--   req_kv_type_k         `-ctk` KV cache dtype ("f16"|"q8_0"|"q4_0"). NULL => not specified.
--   req_kv_type_v         `-ctv` KV cache dtype, INDEPENDENT of K. NULL => not specified.
--                         Together these replace the un-persisted kv_quant bool. A legacy
--                         kv_quant=true request is stored as the pair it actually means
--                         ('q4_0','q4_0'), so replay reproduces the same estimate AND flags.
--   req_slots             `-np` parallel server slots. NULL/0 => 1. Admission-relevant: slots
--                         multiply the KV cache, so replaying the wrong count mis-prices the fit.
--   req_spec_mode         speculative mode: ''|'ngram-cache'|'draft-mtp'. NULL => off.
--   req_spec_draft_max    `--spec-draft-n-max`. NULL/0 => omit the flag (engine default).
--   req_allow_cpu_offload 1 => -ngl auto (+ MoE -ncmoe when derivable). Never relaxes admission.
--
-- NOTE on the legacy `kv_quant` bool: it deliberately gets NO column of its own. Storing both a
-- bool and a pair invites them to disagree after a restart; the API-level bool is resolved to the
-- (K,V) pair ONCE at enqueue time (core.startProfile precedence) and only the pair is persisted.

ALTER TABLE queue ADD COLUMN req_kv_type_k         TEXT;                       -- -ctk dtype; NULL => unset
ALTER TABLE queue ADD COLUMN req_kv_type_v         TEXT;                       -- -ctv dtype; NULL => unset
ALTER TABLE queue ADD COLUMN req_slots             INTEGER;                    -- -np slots; NULL/0 => 1
ALTER TABLE queue ADD COLUMN req_spec_mode         TEXT;                       -- ''|ngram-cache|draft-mtp
ALTER TABLE queue ADD COLUMN req_spec_draft_max    INTEGER;                    -- --spec-draft-n-max; NULL/0 => omit
ALTER TABLE queue ADD COLUMN req_allow_cpu_offload INTEGER NOT NULL DEFAULT 0; -- 1 => -ngl auto
