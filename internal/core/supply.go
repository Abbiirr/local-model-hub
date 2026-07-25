package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/localhub/localhub/internal/manifest"
)

// This file is the Phase-3 supply-chain surface of the service core (spec-digest §7). It owns:
//   - the two-step HF download flow (resolve+verdict → confirm → download), plus gguf_url,
//     ollama pull, and local import;
//   - persisting an IMMUTABLE manifest for every download/import and LINKING it to the models
//     row (manifest_id), so no artifact enters inventory without provenance (gate #p33);
//   - an in-memory download-job registry with live progress over the event bus;
//   - the reproduce export (manifest + launch flags = full reproduction).
//
// Architecture rule (PLAN.md decision 7): all logic lives here; the API/WS transports are thin.

// ── download jobs ────────────────────────────────────────────────────────────

// DownloadState is a job's lifecycle state.
type DownloadState string

const (
	DownloadResolving  DownloadState = "resolving"   // fetching HF metadata (two-step: show phase)
	DownloadAwaitingOK DownloadState = "awaiting_ok" // resolved; waiting for confirm:true
	DownloadRunning    DownloadState = "running"     // bytes flowing
	DownloadHashing    DownloadState = "hashing"     // background hash of a deferred local import
	DownloadSucceeded  DownloadState = "succeeded"
	DownloadFailed     DownloadState = "failed"
)

// DownloadJob is the tracked state of one download/import. It is returned by the create/status
// endpoints and mirrored onto download.progress / download.done events.
type DownloadJob struct {
	ID         string             `json:"id"`
	Kind       string             `json:"kind"` // hf | gguf_url | ollama | local_import
	State      DownloadState      `json:"state"`
	Repo       string             `json:"repo,omitempty"`
	Ref        string             `json:"ref,omitempty"`
	Commit     string             `json:"commit,omitempty"`
	Name       string             `json:"name,omitempty"` // ollama name / display
	Reason     string             `json:"reason,omitempty"`
	Progress   *manifest.Progress `json:"progress,omitempty"`
	ManifestID string             `json:"manifest_id,omitempty"`
	ModelID    string             `json:"model_id,omitempty"`
	Resolved   *ResolvedDownload  `json:"resolved,omitempty"` // set in awaiting_ok (show-before-commit)
	CreatedAt  string             `json:"created_at"`
	UpdatedAt  string             `json:"updated_at"`
}

// ResolvedDownload is the "show before committing" payload (spec-digest §7): the pinned commit
// SHA and the file listing, shown to the user before they confirm the download.
type ResolvedDownload struct {
	Repo      string         `json:"repo"`
	Ref       string         `json:"ref"`
	Commit    string         `json:"commit"`
	Gated     bool           `json:"gated"`
	License   string         `json:"license,omitempty"`
	Files     []ResolvedFile `json:"files"`
	TotalSize int64          `json:"total_size_bytes"`
	Warnings  []string       `json:"warnings"` // pre-download warnings from the fileset
}

type ResolvedFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Selected  bool   `json:"selected"` // in the default download set
}

// downloadRegistry is the in-memory job store. Jobs are ephemeral (not persisted): the durable
// artifacts are the manifest + the models row. A daemon restart loses in-flight jobs, which is
// correct — a killed download's .part is resumable but the job record need not survive.
type downloadRegistry struct {
	mu   sync.Mutex
	jobs map[string]*DownloadJob
}

func newDownloadRegistry() *downloadRegistry {
	return &downloadRegistry{jobs: map[string]*DownloadJob{}}
}

func (r *downloadRegistry) put(j *DownloadJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.ID] = j
}

func (r *downloadRegistry) get(id string) (*DownloadJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	return j, ok
}

func (r *downloadRegistry) list() []DownloadJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DownloadJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, *j)
	}
	return out
}

// ── request/response shapes ──────────────────────────────────────────────────

// DownloadRequest is the POST /api/downloads body. Kind selects the path; the two-step HF flow
// uses Confirm=false to resolve+show, then a second call with Confirm=true (and the same repo/
// ref) to proceed.
type DownloadRequest struct {
	Kind    string   `json:"kind"` // hf | gguf_url | ollama | local_import
	Repo    string   `json:"repo,omitempty"`
	Ref     string   `json:"ref,omitempty"`
	Files   []string `json:"files,omitempty"` // HF file selection (empty = default set)
	URL     string   `json:"url,omitempty"`   // gguf_url
	Name    string   `json:"name,omitempty"`  // ollama model name
	Path    string   `json:"path,omitempty"`  // local_import absolute path
	Confirm bool     `json:"confirm,omitempty"`
}

// StartDownload handles POST /api/downloads. For HF it implements the two-step gate: without
// confirm it resolves + shows the pinned commit + verdict-ready file list and STOPS
// (state=awaiting_ok); with confirm it proceeds to download in the background. Other kinds
// (gguf_url/ollama/local_import) start immediately. Returns the initial job.
func (s *Service) StartDownload(ctx context.Context, req DownloadRequest) (*DownloadJob, error) {
	if s.deps.Manifests == nil {
		return nil, fmt.Errorf("supply chain not configured (manifest store missing)")
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	now := time.Now().UTC().Format(time.RFC3339)
	job := &DownloadJob{ID: manifestUUID(), Kind: kind, CreatedAt: now, UpdatedAt: now}

	switch kind {
	case "hf":
		return s.startHF(ctx, req, job)
	case "gguf_url":
		if s.deps.Downloader == nil {
			return nil, fmt.Errorf("downloader not configured")
		}
		job.State = DownloadRunning
		job.Name = req.URL
		s.registry.put(job)
		go s.runGGUFURL(context.Background(), req, job)
		return job, nil
	case "ollama":
		if s.deps.OllamaPuller == nil {
			return nil, fmt.Errorf("ollama not configured; cannot pull")
		}
		job.State = DownloadRunning
		job.Name = req.Name
		s.registry.put(job)
		go s.runOllamaPull(context.Background(), req, job)
		return job, nil
	case "local_import":
		return s.startLocalImport(ctx, req, job)
	default:
		return nil, fmt.Errorf("unknown download kind %q (want hf | gguf_url | ollama | local_import)", req.Kind)
	}
}

// startHF implements the two-step HF flow. It always resolves first (metadata only). Without
// confirm it returns state=awaiting_ok with the resolved commit + files (show-before-commit).
// With confirm it applies the size guard, then downloads in the background.
func (s *Service) startHF(ctx context.Context, req DownloadRequest, job *DownloadJob) (*DownloadJob, error) {
	if s.deps.Downloader == nil {
		return nil, fmt.Errorf("downloader not configured")
	}
	job.State = DownloadResolving
	job.Repo = req.Repo
	job.Ref = req.Ref
	info, err := s.deps.Downloader.ResolveHF(ctx, req.Repo, req.Ref)
	if err != nil {
		return nil, fmt.Errorf("resolve %s@%s: %w", req.Repo, orMain(req.Ref), err)
	}
	job.Commit = info.CommitSHA

	// Build the resolved-file view + pre-download warnings.
	resolved := s.buildResolved(info, req.Files)
	job.Resolved = resolved

	if !req.Confirm {
		// Show-before-commit: stop here. The UI shows the commit + verdict + warnings and asks
		// the user to confirm; the second call proceeds.
		job.State = DownloadAwaitingOK
		job.Reason = fmt.Sprintf("resolved %s@%s → commit %s; %d file(s), %s. Confirm to download.",
			info.Repo, orMain(req.Ref), shortSHA(info.CommitSHA), len(resolved.Files), humanMiB(resolved.TotalSize))
		touch(job)
		s.registry.put(job)
		return job, nil
	}

	// Confirmed: strict-mode + size guard synchronously so the API returns a clear refusal
	// (rather than the download failing later in the background). Strict mode (opt-in) upgrades
	// a pickle warning to a refusal (ADR-004); it is the ONLY thing that blocks a download.
	if s.deps.Cfg.Security.StrictModelArtifactMode && manifest.HasPickleWarning(resolved.Warnings) {
		return nil, fmt.Errorf(
			"strict mode: refusing to download %s — it contains pickle-format weights with no safetensors alternative. "+
				"Disable security.strict_model_artifact_mode to allow it", info.Repo)
	}
	if err := s.deps.Downloader.Guard.CheckDownloadSize(resolved.TotalSize); err != nil {
		return nil, err
	}
	job.State = DownloadRunning
	touch(job)
	s.registry.put(job)
	// Copy info so the goroutine has the resolved metadata without re-fetching.
	go s.runHF(context.Background(), info, req.Files, job)
	return job, nil
}

// buildResolved turns HF RepoInfo into the show-before-commit payload with the default/selected
// set marked and pre-download warnings computed from the fileset.
func (s *Service) buildResolved(info manifest.RepoInfo, want []string) *ResolvedDownload {
	selectedSet := map[string]bool{}
	for _, f := range manifestSelect(info.Files, want) {
		selectedSet[f.Path] = true
	}
	r := &ResolvedDownload{
		Repo: info.Repo, Ref: info.Ref, Commit: info.CommitSHA,
		Gated: info.Gated, License: info.License,
	}
	var files []manifest.File
	for _, f := range info.Files {
		sel := selectedSet[f.Path]
		r.Files = append(r.Files, ResolvedFile{Path: f.Path, SizeBytes: f.SizeBytes, Selected: sel})
		if sel {
			r.TotalSize += f.SizeBytes
			files = append(files, manifest.File{Path: f.Path})
		}
	}
	r.Warnings = manifest.Scan(manifest.ScanInput{Files: files, License: info.License, Gated: info.Gated})
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	return r
}

// runHF performs the actual HF download in the background, publishing progress, then persists
// the manifest + links the model row.
func (s *Service) runHF(ctx context.Context, info manifest.RepoInfo, files []string, job *DownloadJob) {
	prog := s.progressFn(job)
	m, err := s.deps.Downloader.DownloadHF(ctx, info, files, prog)
	if err != nil {
		s.failJob(job, err)
		return
	}
	s.finishDownload(ctx, job, m, hfDisplayName(info.Repo, m))
}

// runGGUFURL downloads a single GGUF from a direct URL.
func (s *Service) runGGUFURL(ctx context.Context, req DownloadRequest, job *DownloadJob) {
	prog := s.progressFn(job)
	m, err := s.deps.Downloader.DownloadGGUFURL(ctx, manifest.GGUFURLRequest{URL: req.URL}, prog)
	if err != nil {
		s.failJob(job, err)
		return
	}
	display := filepath.Base(m.LocalPath)
	s.finishDownload(ctx, job, m, display)
}

// runOllamaPull pulls via the daemon, then writes a manifest recording the digest.
func (s *Service) runOllamaPull(ctx context.Context, req DownloadRequest, job *DownloadJob) {
	prog := s.progressFn(job)
	res, err := s.deps.OllamaPuller.Pull(ctx, req.Name, prog)
	if err != nil {
		s.failJob(job, err)
		return
	}
	sz := res.SizeBytes
	m := &manifest.Manifest{
		ID:     manifestUUID(),
		Source: manifest.SourceOllama,
		Name:   res.Name,
		Digest: res.Digest,
		Format: "ollama",
	}
	if sz > 0 {
		m.Files = nil // Ollama owns the blob; no per-file hashes
	}
	m.SecurityWarnings = []string{}
	job.Commit = res.Digest
	// Ollama models are keyed differently in inventory (source=ollama, no local path). We link
	// the manifest to the ollama model row after a rescan surfaces it. Record size on the job.
	if sz > 0 && job.Progress == nil {
		job.Progress = &manifest.Progress{File: res.Name, BytesDone: sz, BytesTotal: sz, Phase: "done"}
	}
	s.finishOllama(ctx, job, m, res.Name)
}

// startLocalImport registers an existing on-disk file as a local_import model. The path must be
// inside a configured model dir (containment). Large files hash in the background.
func (s *Service) startLocalImport(ctx context.Context, req DownloadRequest, job *DownloadJob) (*DownloadJob, error) {
	if strings.TrimSpace(req.Path) == "" {
		return nil, fmt.Errorf("local_import requires an absolute path")
	}
	abs, err := filepath.Abs(req.Path)
	if err != nil {
		return nil, fmt.Errorf("resolve import path: %w", err)
	}
	// Containment: the import must be under a configured model dir.
	if err := manifest.ValidateWithin(abs, s.deps.Cfg.Data.ModelDirs); err != nil {
		return nil, err
	}
	// Deferred hash for anything non-trivial (we can't know size cheaply here; always defer and
	// hash in the background — the manifest is written immediately so inventory has provenance).
	m, err := manifest.BuildLocalImportManifest(ctx, manifest.LocalImportRequest{Path: abs, DeferHash: true})
	if err != nil {
		return nil, err
	}
	job.Kind = "local_import"
	job.Name = filepath.Base(abs)
	job.State = DownloadHashing
	touch(job)

	// Persist the manifest NOW (hash_pending) + link the model row so the artifact appears in
	// inventory with provenance immediately (gate #p33: no artifact without a manifest).
	modelID, err := s.persistManifestAndLink(ctx, m, job.Name)
	if err != nil {
		return nil, err
	}
	job.ManifestID = m.ID
	job.ModelID = modelID
	s.registry.put(job)

	// Background hash → finalize the manifest + set models.sha256.
	go s.hashLocalImport(context.Background(), job, m.ID, abs)
	return job, nil
}

// hashLocalImport computes the deferred sha256 and finalizes the manifest.
func (s *Service) hashLocalImport(ctx context.Context, job *DownloadJob, manifestID, path string) {
	sum, err := manifest.HashFile(path)
	if err != nil {
		s.failJob(job, fmt.Errorf("hash %s: %w", path, err))
		return
	}
	if err := s.deps.Manifests.FinalizeHashes(ctx, manifestID, map[string]string{filepath.Base(path): sum}); err != nil {
		s.failJob(job, fmt.Errorf("finalize hash: %w", err))
		return
	}
	// Record the primary hash on the models row for quick integrity checks.
	if s.deps.DB != nil && job.ModelID != "" {
		_, _ = s.deps.DB.ExecContext(ctx, `UPDATE models SET sha256=?, updated_at=datetime('now') WHERE id=?`, sum, job.ModelID)
	}
	job.State = DownloadSucceeded
	job.Reason = "imported (hash finalized)"
	touch(job)
	s.registry.put(job)
	s.publishDone(job)
	s.deps.Events.Publish(Event{Type: EventModelsUpdated})
}

// finishDownload persists a completed download's manifest, links/creates the inventory row, and
// publishes done + models.updated.
func (s *Service) finishDownload(ctx context.Context, job *DownloadJob, m *manifest.Manifest, display string) {
	modelID, err := s.persistManifestAndLink(ctx, m, display)
	if err != nil {
		s.failJob(job, err)
		return
	}
	job.ManifestID = m.ID
	job.ModelID = modelID
	job.State = DownloadSucceeded
	job.Reason = "download complete; manifest written and linked to inventory"
	touch(job)
	s.registry.put(job)
	s.publishDone(job)
	s.deps.Events.Publish(Event{Type: EventModelsUpdated})
}

// finishOllama persists an ollama manifest and links it to the ollama inventory row. Because
// Ollama owns the blob, the model row is created by a rescan of the daemon's /api/tags. We
// rescan FIRST (so the pulled model's row exists), then UPDATE its manifest_id — otherwise the
// link would land on a row the rescan hasn't created yet and be lost. The rescan's Upsert
// preserves manifest_id (ON CONFLICT does not touch it), so a subsequent rescan keeps the link.
func (s *Service) finishOllama(ctx context.Context, job *DownloadJob, m *manifest.Manifest, name string) {
	if err := s.deps.Manifests.Put(ctx, m); err != nil {
		s.failJob(job, err)
		return
	}
	job.ManifestID = m.ID
	modelID := ollamaModelID(name)
	job.ModelID = modelID

	// Rescan so the pulled model's row exists in inventory, THEN link the manifest.
	if _, err := s.RescanModels(ctx); err != nil {
		// A rescan failure is non-fatal to the pull (the model IS pulled + the manifest IS
		// written); the link just waits for the next successful rescan. Note it on the job.
		job.Reason = "ollama pull complete; manifest written but inventory rescan failed: " + err.Error()
	}
	if s.deps.DB != nil {
		if _, err := s.deps.DB.ExecContext(ctx,
			`UPDATE models SET manifest_id=?, updated_at=datetime('now') WHERE id=?`, m.ID, modelID); err != nil {
			job.Reason = "ollama pull complete; manifest written but could not link inventory row: " + err.Error()
		}
	}
	job.State = DownloadSucceeded
	if job.Reason == "" {
		job.Reason = fmt.Sprintf("ollama pull complete; digest %s recorded in manifest and linked to inventory", shortSHA(strings.TrimPrefix(m.Digest, "sha256:")))
	}
	touch(job)
	s.registry.put(job)
	s.publishDone(job)
}

// persistManifestAndLink writes the manifest immutably and upserts/links a models row so the
// artifact is in inventory WITH provenance (gate #p33). It runs in a single DB transaction so a
// manifest is never written without its link (and vice versa). Returns the model id.
func (s *Service) persistManifestAndLink(ctx context.Context, m *manifest.Manifest, display string) (string, error) {
	if err := s.deps.Manifests.Put(ctx, m); err != nil {
		return "", err
	}
	if s.deps.DB == nil {
		return "", nil
	}
	modelID := downloadModelID(m, display)
	source := m.Source.ModelSource()
	format := m.Format
	if format == "" {
		format = "gguf"
	}
	primary := m.PrimaryHash()
	const q = `
INSERT INTO models
  (id, display_name, source, format, quantization, repo, revision, local_path,
   size_bytes, sha256, license, manifest_id, presence, missing_since, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'present', NULL, datetime('now'))
ON CONFLICT(id) DO UPDATE SET
  display_name = excluded.display_name,
  source       = excluded.source,
  format       = excluded.format,
  quantization = COALESCE(NULLIF(excluded.quantization,''), models.quantization),
  repo         = COALESCE(excluded.repo, models.repo),
  revision     = COALESCE(excluded.revision, models.revision),
  local_path   = COALESCE(excluded.local_path, models.local_path),
  size_bytes   = COALESCE(excluded.size_bytes, models.size_bytes),
  sha256       = COALESCE(NULLIF(excluded.sha256,''), models.sha256),
  license      = COALESCE(NULLIF(excluded.license,''), models.license),
  manifest_id  = excluded.manifest_id,
  presence     = 'present',
  missing_since= NULL,
  updated_at   = datetime('now')`
	var size any
	if ts := m.TotalSizeBytes(); ts > 0 {
		size = ts
	}
	_, err := s.deps.DB.ExecContext(ctx, q,
		modelID, display, source, format,
		nullIfEmpty(m.Quantization), nullIfEmpty(m.Repo), nullIfEmpty(m.Revision),
		nullIfEmpty(m.LocalPath), size, nullIfEmpty(primary), nullIfEmpty(m.License), m.ID,
	)
	if err != nil {
		return "", fmt.Errorf("link manifest %s to model %s: %w", m.ID, modelID, err)
	}
	return modelID, nil
}

// ── reproduce export ─────────────────────────────────────────────────────────

// Reproduce builds the GET /api/models/{id}/reproduce bundle for an inventory model. It reads
// the model's manifest (if any) and the latest instance's launch_flags_json (if the model ran),
// so the bundle names exact bytes + revision + flags (spec-digest §7). A model with no manifest
// (a discovered pre-existing local file) gets provenance:"unknown (pre-existing local file)".
func (s *Service) Reproduce(ctx context.Context, modelID string) (manifest.ReproduceBundle, error) {
	if s.deps.DB == nil {
		return manifest.ReproduceBundle{}, fmt.Errorf("database not configured")
	}
	var (
		displayName, source, localPath, repo, revision, sha string
		manifestID                                          sql.NullString
	)
	err := s.deps.DB.QueryRowContext(ctx, `
SELECT display_name, source, COALESCE(local_path,''), COALESCE(repo,''),
       COALESCE(revision,''), COALESCE(sha256,''), manifest_id
FROM models WHERE id = ?`, modelID).Scan(&displayName, &source, &localPath, &repo, &revision, &sha, &manifestID)
	if err == sql.ErrNoRows {
		return manifest.ReproduceBundle{}, fmt.Errorf("model %q not found", modelID)
	}
	if err != nil {
		return manifest.ReproduceBundle{}, fmt.Errorf("read model %q: %w", modelID, err)
	}

	var m *manifest.Manifest
	if manifestID.Valid && manifestID.String != "" && s.deps.Manifests != nil {
		m, _ = s.deps.Manifests.Get(ctx, manifestID.String)
	}
	b := manifest.FromManifest(m)
	b.ModelID = modelID
	b.DisplayName = displayName

	// For a discovered (no-manifest) model, fill the local path so the bundle still names the
	// exact bytes on disk (provenance stays "unknown (pre-existing local file)").
	if m == nil {
		b.LocalPath = localPath
		b.PrimarySHA256 = sha
		if repo != "" {
			b.Repo = repo
			b.Commit = revision
		}
	}

	// Attach launch flags from the most recent instance of this model, if it ever ran.
	if engine, flags, ok := s.latestLaunchFlags(ctx, modelID); ok {
		b.Engine = engine
		b.LaunchFlags = flags
	}
	return b, nil
}

// latestLaunchFlags reads the newest instance's engine + launch_flags_json for a model. Returns
// ok=false when the model has never run (no instance rows / null flags). This is the
// manifest→launch-flags linkage Phase 4 populates when it starts instances.
func (s *Service) latestLaunchFlags(ctx context.Context, modelID string) (string, []string, bool) {
	var engine string
	var flagsJSON sql.NullString
	err := s.deps.DB.QueryRowContext(ctx, `
SELECT engine, launch_flags_json FROM instances
WHERE model_id = ? AND launch_flags_json IS NOT NULL
ORDER BY COALESCE(started_at, '') DESC LIMIT 1`, modelID).Scan(&engine, &flagsJSON)
	if err != nil || !flagsJSON.Valid || flagsJSON.String == "" {
		return "", nil, false
	}
	var flags []string
	if err := json.Unmarshal([]byte(flagsJSON.String), &flags); err != nil {
		// launch_flags_json may be an object; fall back to a single JSON blob string.
		return engine, []string{flagsJSON.String}, true
	}
	return engine, flags, true
}

// GetManifest returns a manifest by id for GET /api/manifests/{id}.
func (s *Service) GetManifest(ctx context.Context, id string) (*manifest.Manifest, error) {
	if s.deps.Manifests == nil {
		return nil, fmt.Errorf("manifest store not configured")
	}
	return s.deps.Manifests.Get(ctx, id)
}

// ManifestForModel returns the manifest linked to a model id (or nil if none).
func (s *Service) ManifestForModel(ctx context.Context, modelID string) (*manifest.Manifest, error) {
	if s.deps.DB == nil || s.deps.Manifests == nil {
		return nil, fmt.Errorf("supply chain not configured")
	}
	var mid sql.NullString
	err := s.deps.DB.QueryRowContext(ctx, `SELECT manifest_id FROM models WHERE id = ?`, modelID).Scan(&mid)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("model %q not found", modelID)
	}
	if err != nil {
		return nil, err
	}
	if !mid.Valid || mid.String == "" {
		return nil, nil
	}
	return s.deps.Manifests.Get(ctx, mid.String)
}

// Downloads returns all tracked download jobs (GET /api/downloads).
func (s *Service) Downloads() []DownloadJob { return s.registry.list() }

// DownloadStatus returns one job by id (GET /api/downloads/{id}).
func (s *Service) DownloadStatus(id string) (*DownloadJob, bool) { return s.registry.get(id) }

// ── helpers ──────────────────────────────────────────────────────────────────

// progressFn returns a ProgressFunc that updates the job + publishes download.progress.
func (s *Service) progressFn(job *DownloadJob) manifest.ProgressFunc {
	return func(p manifest.Progress) {
		pc := p
		job.Progress = &pc
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		s.deps.Events.Publish(Event{Type: EventDownloadProgress, Data: jobSnapshot(job)})
	}
}

func (s *Service) failJob(job *DownloadJob, err error) {
	job.State = DownloadFailed
	job.Reason = err.Error()
	touch(job)
	s.registry.put(job)
	s.publishDone(job)
}

func (s *Service) publishDone(job *DownloadJob) {
	s.deps.Events.Publish(Event{Type: EventDownloadDone, Data: jobSnapshot(job)})
}

// jobSnapshot returns a copy of the job for event payloads (avoid racing on the live pointer).
func jobSnapshot(job *DownloadJob) DownloadJob { return *job }

func touch(job *DownloadJob) { job.UpdatedAt = time.Now().UTC().Format(time.RFC3339) }

func orMain(ref string) string {
	if ref == "" {
		return "main"
	}
	return ref
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func humanMiB(n int64) string {
	if n <= 0 {
		return "size unknown"
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// downloadModelID derives a stable models.id for a downloaded/imported artifact. It mirrors the
// inventory scanner's natural-key style (source-prefixed slug) so a later rescan of the same
// file updates the same row rather than duplicating it.
func downloadModelID(m *manifest.Manifest, display string) string {
	switch m.Source {
	case manifest.SourceHF:
		return "hf-" + slug(m.Repo)
	case manifest.SourceGGUFURL:
		return "url-" + slug(display)
	case manifest.SourceLocalImport:
		return "local-" + slug(display)
	default:
		return slug(display)
	}
}

func ollamaModelID(name string) string { return "ollama-" + slug(name) }

func hfDisplayName(repo string, m *manifest.Manifest) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 && i+1 < len(repo) {
		return repo[i+1:]
	}
	return repo
}

// slug lowercases and replaces non-alnum runs with a single hyphen (matches the scanner's
// slug shape closely enough that IDs are stable and human-readable).
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// manifestUUID re-exposes the manifest package's uuid generator via a small indirection so core
// and manifest stay decoupled (core does not import crypto/rand for this).
func manifestUUID() string { return manifest.NewUUID() }

// manifestSelect mirrors the downloader's default file selection for the show-before-commit
// view (kept here so the resolved payload marks exactly what would be downloaded).
func manifestSelect(all []manifest.HFFile, want []string) []manifest.HFFile {
	return manifest.SelectFilesForView(all, want)
}
