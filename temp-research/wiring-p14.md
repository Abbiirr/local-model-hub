# P1.4 Inventory — wiring notes (for the wiring agent + orchestrator sign-off)

Package delivered: `internal/inventory/` — model scanner + SQLite persistence, no HTTP, no
engine clients. `go vet`, `go test`, and `go build ./...` all clean (whole module). Real-box
scan of `K:\projects\llm_models\models` verified (summary at bottom).

This package touches ONLY `internal/inventory/` + this file. It imports `internal/db`
READ-ONLY in tests (via `db.Open` + `db.Migrate`); it does not modify db/core/api/config.

---

## 1. What the wiring agent must connect

### 1a. Construct the scanner + repository, inject into `core.Deps`

`internal/core/core.go` has a `Deps` struct with a documented seam:
```go
// Future seams (P1.2+): Hardware probe, engine adapters, inventory scanner, ...
```
Add one field and construct it in the daemon bootstrap (cmd/localhub) — NOT in this package:

```go
// in core.Deps:
Inventory *inventory.Scanner   // disk+ollama scanner
InvRepo   *inventory.Repository // upsert/reconcile over *db.DB

// in bootstrap (cmd/localhub), after cfg + db are ready:
scanner := inventory.New(
    cfg.Data.ModelDirs,                       // config mapping — see §3
    inventory.WithOllama(ollamaListerAdapter),// optional; see §4
)
invRepo := inventory.NewRepository(db)        // *db.DB satisfies inventory.DBHandle
```
`*db.DB` embeds `*sql.DB`, so it satisfies `inventory.DBHandle`
(`ExecContext/QueryContext/QueryRowContext`) AND the internal `txBeginner` (promoted
`BeginTx`) — the repository will run each `Upsert` in a real transaction automatically.

### 1b. Core methods to add (in `internal/core/core.go`, thin over the package)

Two methods, matching the spec-digest §3 API surface:

```go
// Models returns the current inventory snapshot. GET /api/models.
// It reads from the DB (the catalog), NOT by re-walking disk on every request — the
// walk is done by RescanModels. For P1.4/P1.5 read-only milestone, a straight SELECT of
// the `models` table into a ModelSummary slice is enough. (A thin query lives best in
// internal/db or a small core query; inventory package intentionally does not own reads
// beyond Count — it owns discovery + upsert.)
func (s *Service) Models(ctx context.Context) ([]ModelSummary, error)

// RescanModels performs a fresh scan and upserts into the catalog, then reconciles
// vanished rows. POST /api/models/rescan.
func (s *Service) RescanModels(ctx context.Context) (RescanResult, error) {
    res := s.deps.Inventory.Scan(ctx)          // ScanResult{Models, Skipped, Errors}
    // log res.Skipped (partials/orphans) and res.Errors — do NOT drop silently.
    stats, err := s.deps.InvRepo.Upsert(ctx, res.Models)
    if err != nil { return RescanResult{}, err }
    rec, err := s.deps.InvRepo.Reconcile(ctx, res.Models, scannedSources(res))
    // decide missing-row policy here (see §5) — default: report, don't delete/mark.
    return RescanResult{Inserted: stats.Inserted, Updated: stats.Updated,
        Vanished: rec.VanishedIDs, Skipped: res.Skipped}, err
}
```
`scannedSources(res)` = the set of `Source` values actually covered this pass
(`local_path`, `huggingface` always; `ollama` only if the Ollama lister was wired AND did
not error). Pass that to `Reconcile` so it never marks rows from an un-scanned source as
vanished (e.g. when Ollama is down).

### 1c. Routes to register (in `internal/api/server.go` `routes()`)

Per spec-digest §3 "Models (inventory)":
- `GET  /api/models`         -> `svc.Models(ctx)` -> `[]ModelSummary` (normalized inventory).
- `POST /api/models/rescan`  -> `svc.RescanModels(ctx)`. **Note:** spec-digest §3 lists
  `POST /api/models/import` (Phase 3, download) and `DELETE /api/models/{id}`, but does NOT
  list an explicit rescan endpoint. Rescan is implied by the "re-scan is idempotent" gate
  (TESTING.md#p14) and is the natural trigger. Register it under `/api/models/rescan`
  (admin-only per security.md — it walks disk); confirm the exact path with the orchestrator
  if a different convention is preferred. The first-milestone playbook only requires
  `GET /api/models` to exist; rescan can run on daemon startup too.

Also: run one scan+upsert on daemon startup (after migrations) so `GET /api/models` returns
real data on first boot — cheap (header sniff only, no hashing).

---

## 2. Record shape ↔ `models` DDL mapping (spec-digest §2 / migration 0001)

`inventory.Model` maps 1:1 onto the persisted columns. The repository writes only the
scanner-owned columns and PRESERVES the rest on re-scan:

| models column        | inventory.Model field           | notes |
|----------------------|---------------------------------|-------|
| id (PK)              | `ID`                            | deterministic natural key (see §6) |
| display_name (NOTNULL)| `DisplayName`                  | filename stem / dir base / ollama name |
| source (NOTNULL)     | `Source`                        | ollama \| huggingface \| local_path |
| format (NOTNULL)     | `Format`                        | gguf \| safetensors \| pytorch \| ollama |
| quantization         | `Quantization`                  | "" → NULL |
| repo                 | `Repo`                          | "" → NULL (HF repo id if derivable) |
| revision             | `Revision`                      | "" → NULL; only set if real refs metadata |
| local_path           | `LocalPath`                     | file for GGUF, dir for safetensors |
| size_bytes           | `SizeBytes *int64`              | nil → NULL |
| sha256               | `SHA256 *string`                | **always nil for disk artifacts** (§5 hash policy); ollama digest recorded |
| license              | `License`                       | "present" marker when a LICENSE file exists |
| recommended_engine   | — (not scanner-owned)           | PRESERVED on re-scan |
| compat_status        | — (not scanner-owned)           | PRESERVED on re-scan |
| manifest_id          | — (not scanner-owned)           | PRESERVED on re-scan (Phase 3) |
| created_at           | — (DB default on insert)        | untouched on update |
| updated_at           | — (bumped every write)          | freshness signal for idempotent re-scan |

`Upsert` uses `INSERT ... ON CONFLICT(id) DO UPDATE` and does NOT list
recommended_engine/compat_status/manifest_id/created_at in the SET clause, so a bare
inventory re-scan can never clobber the compat engine's or supply-chain phase's writes.
`license` uses `COALESCE(NULLIF(excluded,''), existing)` and `sha256` uses
`COALESCE(excluded, existing)` for the same reason.

---

## 3. Config mapping

- **Scan roots:** `cfg.Data.ModelDirs` ([]string; default `[K:\projects\llm_models\models]`).
  Each dir is walked for BOTH loose GGUFs and HF/safetensors model roots. **There is no
  separate HF-dirs config key** — one `model_dirs` list holds all local sources. If you want
  HF and GGUF roots configured separately later, that's an additive config change (out of
  P1.4 scope); the scanner already handles both under one root.
- **Ollama source:** `cfg.Engines.Ollama.Enabled` + `.BaseURL`. Only wire `WithOllama(...)`
  when Ollama is enabled AND the engines client is available. If disabled/unreachable, omit
  it — the disk scan still runs, and Reconcile won't touch ollama rows.

---

## 4. OllamaLister adapter needed from the engines package

The scanner consumes a consumer-side interface (defined in `internal/inventory/ollama.go`):
```go
type OllamaLister interface { ListModels(ctx context.Context) ([]inventory.OllamaModel, error) }
type OllamaModel struct { Name, Digest string; SizeBytes int64; Family, Quant string }
```
The real client `internal/engines/ollama.Client` already has:
```go
func (c *Client) ListModels(ctx) ([]ollama.TagModel, error)
// TagModel{ Name string; Size int64; Digest string; Details ModelDetails }
// ModelDetails{ Family string; QuantizationLevel string; ... }
```
So the wiring agent writes a ~10-line adapter (place it in cmd/localhub or a small wiring
package — NOT in internal/inventory, NOT in internal/engines, to keep both agents' packages
untouched):
```go
type ollamaInvAdapter struct{ c *ollama.Client }
func (a ollamaInvAdapter) ListModels(ctx context.Context) ([]inventory.OllamaModel, error) {
    tags, err := a.c.ListModels(ctx)
    if err != nil { return nil, err }
    out := make([]inventory.OllamaModel, 0, len(tags))
    for _, t := range tags {
        out = append(out, inventory.OllamaModel{
            Name: t.Name, Digest: t.Digest, SizeBytes: t.Size,
            Family: t.Details.Family, Quant: t.Details.QuantizationLevel,
        })
    }
    return out, nil
}
```
Field shapes verified against `internal/engines/ollama/ollama.go` as it stands today. Note
spec-digest §13(8): Ollama API shapes are time-sensitive — the engines agent owns
re-verification; the adapter is a pure field copy so it stays correct as long as TagModel does.

---

## 5. Decisions needing orchestrator sign-off

### (A) Hash policy: sha256 DEFERRED — NOT computed by the scanner. [needs sign-off]
- **Decision:** the inventory walk records `sha256 = NULL` for every disk artifact. Cheap
  identity for change-detection is `(path, size, mtime)`. A real content hash is produced
  ONLY by the download/import path (`internal/manifest`, Phase 3), never by a background
  inventory scan.
- **Why:** the reference box has multiple 13–14 GB GGUFs. Hashing them on every scan would
  read ~100 GB of disk per pass — unacceptable for a routine catalog refresh, and it buys
  nothing P1.4 needs (the natural key + size/mtime already detect change). Ollama's
  already-computed manifest digest IS recorded (free) into `sha256`.
- **Consequence for the orchestrator:** the P1.4 gate (TESTING.md#p14) says "hash where
  available" — Ollama digests ARE available and stored; disk GGUF hashes are intentionally
  deferred. Confirm this reading. Phase 3 (`internal/manifest`) is where per-file sha256 is
  computed and verified at download time (spec §7); it can backfill `models.sha256` then.

### (B) Missing/vanished rows: REPORTED, not deleted, not marked — because the DDL lacks a
### presence column. [needs sign-off + likely a future additive migration]
- **DDL mismatch (see §7):** the `models` table has NO status/presence/missing column.
  `compat_status` exists but has DIFFERENT semantics (runs | runs_degraded | wont_run |
  unknown) — overloading it with "missing" corrupts the compat vocabulary.
- **Decision:** `Repository.Reconcile(ctx, seen, scannedSources)` returns the set of
  `VanishedIDs` (catalog rows in a scanned source that this scan did not find) and does NOT
  delete or mutate them. The caller/orchestrator decides policy.
- **Interim opt-in provided:** `Repository.MarkMissingViaCompatStatus(ctx, ids)` sets
  `compat_status='missing'` if the orchestrator explicitly wants a persisted marker before a
  proper column exists. NOT called by default.
- **Recommended real fix:** an additive migration adding e.g. `presence TEXT` (present|
  missing) + `missing_since TEXT` to `models` (data-model.md permits additive columns). That
  migration belongs to the db agent, not inventory. Until then, default behavior = report
  vanished ids and let the API surface "N models no longer on disk" without touching rows.

---

## 6. Natural key (models.id) design

Deterministic per artifact so re-scan updates instead of duplicating:
- GGUF: `gguf-<slug(path-rel-to-root, minus .gguf and shard suffix)>` — multi-part shards
  collapse to one id.
- HF/safetensors dir: `hf-<slug(dir-rel-to-root)>`.
- Ollama: `ollama-<slug(model name)>`, e.g. `llama3.1:8b` → `ollama-llama3-1-8b`.
Keys derive ONLY from stable identity (never size/mtime), so a file that merely changed size
keeps its id and updates its row.

---

## 7. DDL mismatches / gaps found (report only — no db edits made)

1. **No presence/missing column on `models`.** Blocks a clean "mark vanished as missing"
   per the P1.4 spec. Worked around via return-value reporting (§5B). Recommend additive
   `presence` + `missing_since` columns.
2. **No column for `architectures` / `model_type`.** The scanner captures these from a
   safetensors `config.json` (needed by the compat engine, P2.2) but there is nowhere in
   `models` to persist them. Carried on `inventory.Model.{Architectures,ModelType}` and NOT
   persisted. Options for the orchestrator: additive `arch`/`model_type` columns, or stash
   them in the manifest (Phase 3), or a side table. Flagged, not decided.
3. **No column for companions (mmproj) or multi-part `parts`.** The scanner links mmproj
   projectors and multi-part shard sets (needed for launch flags: llama.cpp needs `--mmproj`
   and the shard-0 path). Carried on `inventory.Model.{Companions,Parts}`, not persisted.
   Recommend storing these in the manifest `files_json` (Phase 3) or an additive column;
   they are load-bearing for the llama.cpp adapter's launch templates (engine-adapters.md).
4. **No column for scan warnings.** Pickle/orphan/parse warnings are carried on
   `inventory.Model.Warnings` + `ScanResult.Skipped`; download-time warnings have a home
   (`manifests.security_warnings_json`) but scan-time warnings do not. Surface them via the
   API response / logs; they need no `models` column.

None of these block the P1.4 gate; all are additive-migration candidates the db/orchestrator
owns.

---

## 8. Real-box scan result (K:\projects\llm_models\models, read-only, verified)

**9 standalone GGUF models** discovered, **3 artifacts skipped** (2 partials + 1 orphan
mmproj), **0 errors**:

| id | quant | size | companions |
|----|-------|------|------------|
| gguf-qwen3-5-27b-tq3-1s | tq3_1s | 13.88 GB | mmproj-BF16 (shared) |
| gguf-qwen3-6-27b-tq3-4s | tq3_4s | 13.95 GB | mmproj-BF16 (shared) |
| gguf-qwen3-6-35b-a3b-tq3-4s | tq3_4s | 13.30 GB | mmproj-BF16 (shared) |
| gguf-qwopus3-5-27b-v3-tq3-4s | tq3_4s | 13.95 GB | Qwopus…-mmproj (name-match) + mmproj-BF16 |
| gguf-glm-ocr-glm-ocr-q8-0 | q8_0 | 0.95 GB | mmproj-GLM-OCR-Q8_0 |
| gguf-paddleocr-vl-1-5-paddleocr-vl-1-5 | (none) | 0.94 GB | PaddleOCR-VL-1.5-mmproj |
| gguf-qwen3-vl-32b-instruct-q2k-… | q2_k | 12.34 GB | mmproj-F16 |
| gguf-qwen3-vl-embedding-8b-… | q6_k | 6.22 GB | — |
| gguf-nomic-embed-text-v2-moe-… | q4_k_m | 0.34 GB | — |

Skipped (excluded + logged, per TESTING.md#p14):
- `Qwen3.5-27B-TQ3_1S.gguf.tmp` — partial/in-progress download.
- `Qwen3.6-27B-TQ3_4S.gguf.part.aria2` — partial/in-progress download.
- `qwen3.6-27b-tq3_4s\mmproj.gguf` — orphan mmproj (no base GGUF in its directory).

Notes on companion linking: with multiple base GGUFs + multiple mmprojs in one directory,
the scanner uses filename name-affinity (dropping quant/size/version tokens) to attach a
named projector to its model, and links an unnamed/ambiguous projector (`mmproj-BF16.gguf`,
no distinctive tokens) to all bases as a possibly-shared projector. This is a documented
heuristic, not a hard identity claim; correct on this box. TQ3_* quants parse fine at the
FILENAME level (quant label detected) even though gguf-parser cannot parse their metadata
(that's a P2.1 concern, not P1.4). No config.json/safetensors dirs exist on this box, so the
safetensors + pickle paths are covered by fixture tests, not the real-box scan.

Ollama models were NOT included in the real-box run above (the scanner was constructed
without a live OllamaLister to keep the check read-only and engine-independent); the merge
path is covered by `TestScanMergesOllama`.
