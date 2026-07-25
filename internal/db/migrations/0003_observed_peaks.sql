-- 0003_observed_peaks.sql
-- Additive migration (data-model.md: additive only; no destructive migrations). Adds ONE
-- new table, `observed_peaks`, that had no home in 0001/0002. Approved by orchestrator
-- sign-off (P2.5 calibration-store task, 2026-07-06). No existing table/column is touched.
--
-- PURPOSE — the "measured VRAM beats estimate" loop (friction log #6; lmwrapper §3):
--   gguf-parser, Ollama, and NVML report THREE disagreeing VRAM numbers for the same model,
--   and gguf-parser cannot parse the TQ3_* quants this box's flagship models depend on at
--   all. Once a model has actually been loaded and its real NVML VRAM peak measured, that
--   measured value is authoritative and should be preferred over any estimate. This table is
--   where those measured peaks live. `internal/compat.CalibrationStore` reads it (satisfying
--   the gguf package's CalibrationStore interface → Confidence "exact") and records into it.
--
-- COLUMNS (naming aligned with 0001/0002 DDL conventions — snake_case, TEXT timestamps):
--   model_key       TEXT PRIMARY KEY   Stable calibration lookup key. For inventory models
--                                      this is the models.id slug (passed to the estimator via
--                                      gguf.Source.WithModelKey); for ad-hoc/remote keys it is
--                                      the source-derived string. One measured peak per key —
--                                      a later, better measurement UPSERTs (REPLACE) the row.
--   peak_vram_mib   INTEGER NOT NULL   The measured NVML VRAM delta peak in MiB (loaded − baseline),
--                                      i.e. the driver-truth footprint of the model during load +
--                                      one generation. This is the number the estimator returns
--                                      verbatim as an exact estimate.
--   ctx             INTEGER            Context length (tokens) the peak was measured at. Recorded
--                                      because VRAM scales with ctx; a peak at ctx=4096 is not
--                                      directly comparable to a request at ctx=32768. NULL if unknown.
--   kv_type         TEXT               KV cache dtype in force at measurement (f16 | q8_0 | q4_0 |
--                                      tq3_0). The fork's tq3_0 KV is smaller than f16, so the
--                                      dtype qualifies the peak. NULL if unknown.
--   binary_hash     TEXT               Optional hash/identifier of the engine binary that produced
--                                      the peak (e.g. llama-server build id / commit). A different
--                                      binary can have a different footprint; NULL when not captured.
--   gpu             TEXT               GPU model the peak was measured on (e.g. "NVIDIA GeForce
--                                      RTX 4060 Ti"). A peak is only valid for the same GPU class.
--   driver          TEXT               Driver version at measurement (e.g. "576.40"). Recorded for
--                                      provenance / drift detection (docs/22 version-policy spirit).
--   observed_at     TEXT NOT NULL      RFC3339/datetime timestamp of the measurement. Powers the
--                                      verdict note ("observed peak from <date> at ctx N").
--                                      DEFAULT (datetime('now')) so a bare INSERT stamps it.
--
-- The estimator prefers a stored peak ONLY as a full override keyed by model_key (the gguf
-- package's ObservedPeak(modelKey) contract). ctx/kv_type/gpu/driver are provenance for the
-- verdict note and for future ctx-aware calibration; they are not part of the lookup key today.
--
-- SQLite CREATE TABLE is a fast metadata-only change; the migration runner
-- (internal/db/migrate.go) wraps this file in its own transaction.

CREATE TABLE observed_peaks (
  model_key       TEXT PRIMARY KEY,          -- stable calibration key (models.id slug, or source-derived)
  peak_vram_mib   INTEGER NOT NULL,          -- measured NVML VRAM peak (MiB); authoritative footprint
  ctx             INTEGER,                   -- context length the peak was measured at (tokens)
  kv_type         TEXT,                      -- KV cache dtype: f16 | q8_0 | q4_0 | tq3_0
  binary_hash     TEXT,                      -- engine binary id/hash that produced the peak (nullable)
  gpu             TEXT,                      -- GPU model the peak is valid for
  driver          TEXT,                      -- driver version at measurement
  observed_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
