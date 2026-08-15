-- 0011_model_launch_defaults.sql
-- ADDITIVE migration (data-model.md: additive only). New table; nothing existing is touched.
--
-- WHY (M2.3 + M6.1/M6.2): once a launch profile has been PROVEN on this box, re-deriving it by hand
-- for every start is how a validated profile gets quietly lost. This table stores one saved default
-- profile per model so the UI can pre-fill the advanced start form with values known to work,
-- instead of an operator re-typing `-ctk q8_0 -ctv q4_0 --spec-type draft-mtp --spec-draft-n-max 2`.
--
-- TWO DELIBERATE CONSTRAINTS, both from the plan:
--
-- (1) A default is NOT auto-applied by the start path. TODO.md M6.1 says a model default may only be
--     saved after the lower-context live gates pass, and silently rewriting an explicit API start
--     would be exactly the kind of hidden behaviour change that made the queue-replay bug so hard to
--     see (KNOWN-LIMITATIONS #21). So this table is READ by the UI to pre-fill a form the operator
--     confirms, and by `GET /api/models/{id}/launch-default`. `POST /api/instances` keeps honouring
--     precisely what the caller sent. If auto-apply is ever wanted it needs its own ADR and its own
--     gate, because it changes what an UNSPECIFIED field means.
--
-- (2) A default is PROVENANCE-BOUND (M6.2). A profile validated against b10106 and one specific GGUF
--     is not evidence about a different engine build or a re-quantized file — the whole reason
--     capability-first binary selection exists is that the older fork DIED on this model's hybrid SSM
--     architecture. So every row records WHAT it was validated against, and a consumer that finds a
--     mismatch must surface it rather than trusting the row:
--
--       engine_version   llama.cpp build the profile was validated on (e.g. '10106').
--       engine_commit    that build's short commit (e.g. '1425386fd').
--       binary_path      absolute path of the binary, so a human can identify it.
--       artifact_sha256  SHA-256 of the exact GGUF the profile was validated against. NULL when not
--                        yet computed (hashing a 13.7 GB file is not free) — NULL means "provenance
--                        unverified", never "matches".
--       validated_note   free text: what was actually proven, and at what context.
--
-- Columns mirror residency.ProfileSpec plus the context, so a saved default and a queue row speak the
-- same vocabulary (migration 0009). model_id is the PRIMARY KEY: one authoritative saved default per
-- model, newest write wins — the same "one row, newest wins" shape as observed_peaks.

CREATE TABLE IF NOT EXISTS model_launch_defaults (
    model_id              TEXT PRIMARY KEY REFERENCES models(id) ON DELETE CASCADE,

    -- The profile itself (same vocabulary as queue.req_* / residency.ProfileSpec).
    ctx                   INTEGER,  -- context length; NULL => the daemon default
    kv_type_k             TEXT,     -- -ctk dtype: 'f16'|'q8_0'|'q4_0'; NULL => unset
    kv_type_v             TEXT,     -- -ctv dtype, INDEPENDENT of K; NULL => unset
    slots                 INTEGER,  -- -np; NULL/0 => 1
    spec_mode             TEXT,     -- ''|'ngram-cache'|'draft-mtp'
    spec_draft_max        INTEGER,  -- --spec-draft-n-max; NULL/0 => omit
    allow_cpu_offload     INTEGER NOT NULL DEFAULT 0,

    -- Provenance: WHAT this default was validated against (M6.2). A default whose engine build or
    -- artifact hash no longer matches is a stale claim, not a recommendation.
    engine_version        TEXT,
    engine_commit         TEXT,
    binary_path           TEXT,
    artifact_sha256       TEXT,
    validated_note        TEXT,

    created_at            TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at            TEXT NOT NULL DEFAULT (datetime('now'))
);
