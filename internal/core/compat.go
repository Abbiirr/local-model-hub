package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/localhub/localhub/internal/compat"
	"github.com/localhub/localhub/internal/compat/gguf"
	"github.com/localhub/localhub/internal/compat/hfmeta"
	"github.com/localhub/localhub/internal/compat/safetensors"
	"github.com/localhub/localhub/internal/engines"
	"github.com/localhub/localhub/internal/hardware"
)

// legacyKVProfile builds a minimal compat.LaunchProfile for callers that still describe the KV
// cache with the legacy `kv_quant` bool: true maps to q4_0 on BOTH cache sides, which is exactly
// what the pre-profile code did when it passed kvQuant=true to the estimator and the flag builder.
// false leaves both sides empty => f16 (the estimator default). Used by the Ollama and vLLM
// verdict paths, which take no llama.cpp speculative/slot flags.
//
// M2.2: the profile is the ONE description of a launch. Building it here — rather than passing
// loose bools to Evaluate and again to PlanLlamaLaunch — is what keeps the admission footprint
// and the spawned flags from disagreeing.
func legacyKVProfile(reqCtx, batch int, kvQuant bool) compat.LaunchProfile {
	p := compat.LaunchProfile{Ctx: reqCtx, Batch: batch}
	if kvQuant {
		p.KVTypeK = gguf.KVCacheQ4_0
		p.KVTypeV = gguf.KVCacheQ4_0
	}
	return p
}

// startProfile builds the full launch profile for a llama.cpp StartInstance request (M2.1/M2.2):
// the KV cache pair, speculative mode + MTP draft maximum, slot count, CPU-offload permission,
// pin and priority. ONE profile feeds both the verdict and PlanLlamaLaunch, so the admitted
// footprint and the spawned flags cannot diverge.
//
// KV precedence (back-compat contract): an EXPLICIT kv_type_k/kv_type_v wins; if neither is set,
// the legacy `kv_quant` bool applies its historical meaning (true => q4_0 on both sides). Setting
// only ONE side leaves the other at f16 — that is genuinely asymmetric and intended, not a
// mistake, which is exactly what `-ctk q8_0 -ctv f16` means to llama.cpp.
//
// The caller MUST have validated the request first (validateStartProfile); this function assumes
// well-formed values and does no error reporting.
func startProfile(req StartInstanceRequest, reqCtx int) compat.LaunchProfile {
	p := legacyKVProfile(reqCtx, req.Batch, req.KVQuant)
	if req.KVTypeK != "" || req.KVTypeV != "" {
		p.KVTypeK = gguf.KVCacheType(req.KVTypeK)
		p.KVTypeV = gguf.KVCacheType(req.KVTypeV)
	}
	p.Slots = req.Slots
	p.SpecMode = req.Speculative
	p.SpecDraftMax = req.SpecDraftMax
	p.AllowCPUOffload = req.AllowCPUOffload
	p.Embedding = req.Embedding
	p.Pooling = req.Pooling
	p.Reranking = req.Reranking
	p.Pin = req.Pin
	p.Priority = req.Priority
	return p
}

// validateStartProfile validates every profile-bearing field of a start request and returns the
// request with normalized values. Each refusal carries a human-readable reason naming the valid
// set (CLAUDE.md). Validation happens BEFORE any engine dispatch so an invalid value is refused
// identically for llama.cpp, Ollama and vLLM, and before anything is spawned or queued.
func validateStartProfile(req StartInstanceRequest) (StartInstanceRequest, error) {
	spec, err := compat.ValidateSpeculative(req.Speculative)
	if err != nil {
		return req, err
	}
	req.Speculative = spec

	kK, err := compat.ValidateKVCacheType(req.KVTypeK, "k")
	if err != nil {
		return req, err
	}
	kV, err := compat.ValidateKVCacheType(req.KVTypeV, "v")
	if err != nil {
		return req, err
	}
	req.KVTypeK, req.KVTypeV = string(kK), string(kV)

	if req.Slots, err = compat.ValidateSlots(req.Slots); err != nil {
		return req, err
	}
	if req.SpecDraftMax, err = compat.ValidateSpecDraftMax(req.SpecDraftMax); err != nil {
		return req, err
	}
	// A draft maximum without a speculative mode is a contradiction, not a harmless extra: the
	// flag would never be emitted, so silently accepting it would let a caller believe MTP was on.
	if req.SpecDraftMax > 0 && req.Speculative == compat.SpeculativeOff {
		return req, fmt.Errorf("spec_draft_max=%d was requested but speculative is off: --spec-draft-n-max only applies to a "+
			"speculative mode. Set speculative to %q (embedded MTP head) or %q, or drop spec_draft_max",
			req.SpecDraftMax, compat.SpeculativeDraftMTP, compat.SpeculativeNgramCache)
	}

	pooling, err := compat.ValidatePooling(req.Pooling)
	if err != nil {
		return req, err
	}
	req.Pooling = pooling

	// Reranking (`--reranking`) REQUIRES `--embedding --pooling rank` together
	// (tools/server/README.md: "Requires a reranker model ... and the --embedding --pooling rank
	// options"), and compat.LaunchProfile.Normalize enforces that coupling on the profile that
	// actually gets launched — so a reranking request is accepted whether or not the caller also
	// passed embedding/pooling; those are simply overridden to true/"rank". The ONLY thing refused
	// here is an EXPLICIT pooling value that contradicts rank (e.g. reranking:true with
	// pooling:"mean"): silently overriding a caller's explicit, incompatible choice would hide a
	// real mistake, so it is a refusal, not a silent coercion.
	if req.Reranking {
		if req.Pooling != "" && req.Pooling != compat.PoolingRank {
			return req, fmt.Errorf("reranking=true was requested with pooling=%q, but llama.cpp's --reranking requires "+
				"--pooling rank (tools/server/README.md: \"Requires a reranker model ... and the --embedding --pooling "+
				"rank options\"). Set pooling to %q, or drop pooling to let reranking set it automatically",
				req.Pooling, compat.PoolingRank)
		}
		return req, nil
	}

	// A pooling override without embedding mode is a contradiction, not a harmless extra: --pooling
	// only affects an embedding-only server, so silently accepting it here would let a caller
	// believe it changed a generation start's behavior when it changes nothing at all. This check is
	// skipped above when Reranking is set, since Reranking implies embedding regardless of what the
	// caller passed for it.
	if req.Pooling != "" && !req.Embedding {
		return req, fmt.Errorf("pooling=%q was requested but embedding is off: --pooling only affects an embedding-only "+
			"server. Set embedding to true, or drop pooling", req.Pooling)
	}
	return req, nil
}

// CompatEvaluator is the compat rule-engine seam the core depends on (satisfied by
// *compat.Engine). Injected as an interface so core tests can use a fake evaluator without
// the real estimators / network.
type CompatEvaluator interface {
	Evaluate(ctx context.Context, req compat.Request, snap hardware.Snapshot, avail compat.EngineAvailability) compat.Verdict
}

// HFMetaFetcher is the remote-metadata seam (satisfied by *hfmeta.Client). Injected so tests
// can point at a fake HF server without network access.
type HFMetaFetcher interface {
	Fetch(ctx context.Context, repo, revision string) (hfmeta.Result, error)
}

// CompatRequest is the input to CompatPreview (POST /api/compat/preview and the per-model
// GET /api/models/{id}/compat). Exactly ONE selector must be set:
//   - ModelID: an inventory model (read from the catalog; verdict is persisted back).
//   - HFRepo + File: a remote GGUF in an HF repo (gguf-parser range-reads its metadata).
//   - HFRepo (no File): a remote safetensors model (config.json + index.json fetched).
type CompatRequest struct {
	ModelID  string `json:"model_id,omitempty"`
	HFRepo   string `json:"hf_repo,omitempty"`
	File     string `json:"file,omitempty"`     // remote GGUF filename within the repo
	Revision string `json:"revision,omitempty"` // HF commit/branch; "" => main
	Ctx      int    `json:"ctx,omitempty"`      // requested context; 0 => default
	Batch    int    `json:"batch,omitempty"`    // concurrent sequences; 0 => 1
}

// CompatPreview produces a full compatibility Verdict for the requested model against the
// live hardware + detected engines. For an inventory model it also persists the verdict
// summary (status/reason/estimated vram/recommended engine) back onto the `models` row so
// GET /api/models can show a cached badge cheaply.
//
// It returns an error only for a caller-side problem (unconfigured deps, bad request, a
// remote fetch that failed); a model that simply "won't run" is a successful Verdict, not an
// error (spec §4: every model gets a verdict with a reason).
func (s *Service) CompatPreview(ctx context.Context, req CompatRequest) (compat.Verdict, error) {
	if s.deps.Compat == nil {
		return compat.Verdict{}, fmt.Errorf("compat engine not configured")
	}
	if s.deps.Hardware == nil {
		return compat.Verdict{}, fmt.Errorf("hardware probe not configured")
	}

	snap, err := s.deps.Hardware.Snapshot(ctx)
	if err != nil {
		return compat.Verdict{}, fmt.Errorf("read hardware snapshot: %w", err)
	}
	avail := s.engineAvailability(ctx)

	// Resolve the descriptor + estimator sources from the request selector.
	desc, fromInventory, err := s.resolveDescriptor(ctx, req)
	if err != nil {
		return compat.Verdict{}, err
	}

	v := s.deps.Compat.Evaluate(ctx, compat.Request{
		Descriptor:   desc,
		RequestedCtx: req.Ctx,
		Batch:        req.Batch,
	}, snap, avail)

	// When the verdict used a MEASURED observed peak (Confidence "exact"; the compat engine
	// consulted the calibration store), enrich it with the observed_peaks provenance —
	// "observed peak from <date> at ctx N" — which the compat layer cannot compose because it
	// does not read the DB. Best-effort: a missing store/row leaves the verdict unchanged.
	if v.Confidence == compat.ConfidenceExact && s.deps.Calib != nil {
		key := calibKey(req, desc)
		if p, ok, lerr := s.deps.Calib.Lookup(ctx, key); lerr == nil && ok {
			note := fmt.Sprintf("observed peak from %s", p.ObservedAt.Format("2006-01-02"))
			if p.Ctx > 0 {
				note += fmt.Sprintf(" at ctx %d", p.Ctx)
			}
			if p.KVType != "" {
				note += " (" + p.KVType + " KV)"
			}
			v.Warnings = append(v.Warnings, note)
		}
	}

	// Persist the verdict summary for inventory models (cheap cached badge on /api/models).
	if fromInventory && s.deps.DB != nil {
		if perr := s.persistVerdict(ctx, req.ModelID, v); perr != nil {
			// Non-fatal: the verdict is valid even if the cache write fails. Attach a warning.
			v.Warnings = append(v.Warnings, "verdict computed but could not be cached: "+perr.Error())
		}
	}
	return v, nil
}

// calibKey returns the calibration lookup key that matches what the estimator was given via
// gguf.Source.WithModelKey: the inventory model_id for catalog models, else the source-derived
// key (repo/file[@rev] for remote GGUF). Kept in sync with descriptorFromInventory /
// remoteGGUFDescriptor.
func calibKey(req CompatRequest, desc compat.ModelDescriptor) string {
	if req.ModelID != "" {
		return req.ModelID
	}
	if req.HFRepo != "" && req.File != "" {
		key := req.HFRepo + "/" + req.File
		if req.Revision != "" {
			key += "@" + req.Revision
		}
		return key
	}
	return desc.ID
}

// RecordObservedPeak persists a MEASURED VRAM peak into the calibration store (observed_peaks,
// migration 0003). Once recorded, the compat engine prefers this measured value over any
// estimate (Confidence "exact") on the next CompatPreview for the same model key. This is the
// write side of the measured-beats-estimate loop (friction log #6): the P2.5 validation
// harness calls it after each real load, and the Phase 4 engine adapters call it after a
// managed start once the instance's real VRAM footprint is known.
//
// modelKey MUST match the estimator's calibration key: the inventory model_id slug for catalog
// models (see calibKey / descriptorFromInventory). A non-nil error is returned for an
// unconfigured store or an invalid peak (empty key / non-positive MiB).
func (s *Service) RecordObservedPeak(ctx context.Context, p compat.ObservedPeak) error {
	if s.deps.Calib == nil {
		return fmt.Errorf("calibration store not configured")
	}
	return s.deps.Calib.Record(ctx, p)
}

// engineAvailability builds the compat EngineAvailability from the read-only detection sweep.
func (s *Service) engineAvailability(ctx context.Context) compat.EngineAvailability {
	views := s.Engines(ctx)
	var avail compat.EngineAvailability
	for _, v := range views {
		switch v.Name {
		case string(engines.KindOllama):
			avail.OllamaReachable = v.Running
		case string(engines.KindLlamaCPP):
			avail.LlamaBinaries = append(avail.LlamaBinaries, llamaBinaryFromView(v))
		case string(engines.KindVLLM):
			avail.VLLMMode = vllmModeFromView(v)
			avail.VLLMAvailable = v.Installed || v.Running
		}
	}
	return avail
}

// llamaBinaryFromView extracts a compat.LlamaBinary from an engine status view's details
// (backends + capabilities recorded by the llama.cpp detector).
//
// M3.1: the spec/MTP and independent-K/V bits are carried through here so binary SELECTION
// (compat.bestLlamaBinary) and the MTP gate (PlanLlamaLaunch) decide on DETECTED capabilities
// rather than assumptions. SupportsKVQuant was previously hardcoded true for every binary; it
// now reflects the probed `has_kv_cache_types` (both -ctk and -ctv present), and stays true only
// when the detector reported no such key at all (an older status row, or a fake in tests) so no
// existing caller regresses.
func llamaBinaryFromView(v EngineStatusView) compat.LlamaBinary {
	b := compat.LlamaBinary{Detail: v.Reason, Detected: v.Installed, SupportsKVQuant: true}
	if p, ok := v.Details["resolved_path"].(string); ok {
		b.Path = p
	} else if p, ok := v.Details["binary_path"].(string); ok {
		b.Path = p
	}
	// A concise detail label from backends (e.g. "CUDA fork" vs "winget (Vulkan/CPU)").
	// The details map is map[string]bool in-process and map[string]any after a JSON round-trip
	// (the cluster/aggregated path), so both shapes are decoded.
	if caps, ok := v.Details["capabilities"].(map[string]bool); ok {
		b.HasTQ3 = caps["has_tq3"]
		b.HasFlashAttn = caps["has_flash_attn"]
		b.HasSpecMTP = caps["has_spec_mtp"]
		b.HasSpecDraftMax = caps["has_spec_draft_max"]
		if kv, present := caps["has_kv_cache_types"]; present {
			b.SupportsKVQuant = kv
		}
	} else if caps, ok := v.Details["capabilities"].(map[string]any); ok {
		b.HasTQ3 = boolOf(caps["has_tq3"])
		b.HasFlashAttn = boolOf(caps["has_flash_attn"])
		b.HasSpecMTP = boolOf(caps["has_spec_mtp"])
		b.HasSpecDraftMax = boolOf(caps["has_spec_draft_max"])
		if kv, present := caps["has_kv_cache_types"]; present {
			b.SupportsKVQuant = boolOf(kv)
		}
	}
	if backends, ok := v.Details["backends"].([]string); ok {
		b.HasCUDA = containsStr(backends, "CUDA")
		b.Detail = labelBinary(backends, b.HasTQ3)
	} else if backends, ok := v.Details["backends"].([]any); ok {
		var bs []string
		for _, x := range backends {
			if str, ok := x.(string); ok {
				bs = append(bs, str)
			}
		}
		b.HasCUDA = containsStr(bs, "CUDA")
		b.Detail = labelBinary(bs, b.HasTQ3)
	}
	return b
}

func labelBinary(backends []string, hasTQ3 bool) string {
	if hasTQ3 || containsStr(backends, "CUDA") {
		if hasTQ3 {
			return "CUDA fork (has_tq3)"
		}
		return "CUDA build"
	}
	if len(backends) > 0 {
		return strings.Join(backends, "/") + " build"
	}
	return "llama-server"
}

func vllmModeFromView(v EngineStatusView) string {
	if m, ok := v.Details["mode"].(string); ok {
		return m
	}
	return ""
}

func boolOf(v any) bool { b, _ := v.(bool); return b }

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// resolveDescriptor turns a CompatRequest into a ModelDescriptor with resolved estimator
// sources. fromInventory is true when the request named an inventory model_id (so the caller
// persists the verdict). It errors on an ambiguous/empty selector or a failed remote fetch.
func (s *Service) resolveDescriptor(ctx context.Context, req CompatRequest) (compat.ModelDescriptor, bool, error) {
	switch {
	case req.ModelID != "":
		d, err := s.descriptorFromInventory(ctx, req.ModelID)
		return d, true, err
	case req.HFRepo != "" && req.File != "":
		return s.remoteGGUFDescriptor(req), false, nil
	case req.HFRepo != "":
		d, err := s.remoteSafetensorsDescriptor(ctx, req)
		return d, false, err
	default:
		return compat.ModelDescriptor{}, false, fmt.Errorf("compat preview requires one of: model_id, hf_repo+file (remote GGUF), or hf_repo (remote safetensors)")
	}
}

// remoteGGUFDescriptor builds a descriptor for a remote GGUF (gguf-parser range-reads it).
func (s *Service) remoteGGUFDescriptor(req CompatRequest) compat.ModelDescriptor {
	return compat.ModelDescriptor{
		ID:           req.HFRepo + "/" + req.File,
		DisplayName:  req.File,
		Source:       "huggingface",
		Format:       compat.FormatGGUF,
		Quantization: quantFromFilename(req.File),
		Repo:         req.HFRepo,
		Revision:     req.Revision,
		File:         req.File,
		GGUFSource:   gguf.HFRepo(req.HFRepo, req.File, req.Revision),
	}
}

// remoteSafetensorsDescriptor fetches config.json (+ index.json) over HTTP and builds a
// safetensors descriptor. Metadata-only, timeout+size-capped (hfmeta package).
func (s *Service) remoteSafetensorsDescriptor(ctx context.Context, req CompatRequest) (compat.ModelDescriptor, error) {
	client := s.deps.HFMeta
	if client == nil {
		client = &hfmeta.Client{}
	}
	res, err := client.Fetch(ctx, req.HFRepo, req.Revision)
	if err != nil {
		return compat.ModelDescriptor{}, fmt.Errorf("fetch HF metadata: %w", err)
	}
	cfg, err := safetensors.ParseConfig(res.ConfigJSON)
	if err != nil {
		return compat.ModelDescriptor{}, fmt.Errorf("parse config.json for %s: %w", req.HFRepo, err)
	}
	var w safetensors.WeightsInfo
	if len(res.IndexJSON) > 0 {
		if wi, ok := safetensors.ParseIndexTotalSize(res.IndexJSON); ok {
			w = wi
		}
	}
	return compat.ModelDescriptor{
		ID:               req.HFRepo,
		DisplayName:      hfBasename(req.HFRepo),
		Source:           "huggingface",
		Format:           compat.FormatSafetensors,
		Quantization:     string(cfg.Quant()),
		Repo:             req.HFRepo,
		Revision:         req.Revision,
		STConfig:         cfg,
		STWeights:        w,
		STConfigResolved: true,
	}, nil
}

// filesBlob mirrors the models.files_json shape (migration 0002) for reading companions/arch.
type filesBlob struct {
	Companions []struct {
		Kind      string `json:"kind"`
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"companions"`
	Parts         []string `json:"parts"`
	Architectures []string `json:"architectures"`
	ModelType     string   `json:"model_type"`
}

// descriptorFromInventory reads a `models` row and resolves its estimator source from disk
// (a local GGUF file → gguf.LocalFile; a safetensors dir → read config.json + index.json).
func (s *Service) descriptorFromInventory(ctx context.Context, id string) (compat.ModelDescriptor, error) {
	if s.deps.DB == nil {
		return compat.ModelDescriptor{}, fmt.Errorf("database not configured")
	}
	const q = `SELECT display_name, source, format, COALESCE(quantization,''), COALESCE(repo,''),
	       COALESCE(revision,''), COALESCE(local_path,''), files_json
	FROM models WHERE id = ?`
	var displayName, source, format, quant, repo, revision, localPath string
	var filesJSON sql.NullString
	err := s.deps.DB.QueryRowContext(ctx, q, id).Scan(
		&displayName, &source, &format, &quant, &repo, &revision, &localPath, &filesJSON)
	if err == sql.ErrNoRows {
		return compat.ModelDescriptor{}, fmt.Errorf("model %q not found in inventory", id)
	}
	if err != nil {
		return compat.ModelDescriptor{}, fmt.Errorf("read model %q: %w", id, err)
	}

	d := compat.ModelDescriptor{
		ID:           id,
		DisplayName:  displayName,
		Source:       source,
		Format:       compat.Format(format),
		Quantization: quant,
		Repo:         repo,
		Revision:     revision,
		LocalPath:    localPath,
	}

	// Parse companions/arch from files_json (migration 0002).
	var blob filesBlob
	if filesJSON.Valid && filesJSON.String != "" {
		_ = json.Unmarshal([]byte(filesJSON.String), &blob)
		for _, c := range blob.Companions {
			if strings.EqualFold(c.Kind, "mmproj") {
				d.HasMMProj = true
				d.MMProjPath = c.Path
			}
		}
	}

	switch d.Format {
	case compat.FormatGGUF, compat.FormatOllama:
		if localPath != "" {
			d.GGUFSource = gguf.LocalFile(localPath).WithModelKey(id)
		} else if repo != "" {
			// An Ollama or repo-referenced GGUF with no local path: cannot range-read reliably
			// without a file name; leave source zero so the estimator returns unknown with a reason.
			d.GGUFSource = gguf.LocalFile("") // empty → ErrEmptySource → unknown verdict with reason
		}
	case compat.FormatSafetensors, compat.FormatPyTorch:
		cfg, w, ok := loadLocalSafetensors(localPath)
		if ok {
			d.STConfig = cfg
			d.STWeights = w
			d.STConfigResolved = true
		}
	}
	return d, nil
}

// loadLocalSafetensors reads config.json (+ index.json) from a local model directory and
// returns the parsed config + weights info. ok is false if config.json is unreadable.
func loadLocalSafetensors(dir string) (safetensors.ModelConfig, safetensors.WeightsInfo, bool) {
	if dir == "" {
		return safetensors.ModelConfig{}, safetensors.WeightsInfo{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return safetensors.ModelConfig{}, safetensors.WeightsInfo{}, false
	}
	cfg, err := safetensors.ParseConfig(raw)
	if err != nil {
		return safetensors.ModelConfig{}, safetensors.WeightsInfo{}, false
	}
	var w safetensors.WeightsInfo
	if idxRaw, ierr := os.ReadFile(filepath.Join(dir, "model.safetensors.index.json")); ierr == nil {
		if wi, ok := safetensors.ParseIndexTotalSize(idxRaw); ok {
			w = wi
		}
	}
	return cfg, w, true
}

// persistVerdict writes the verdict summary onto the models row (compat_status,
// recommended_engine) and stamps updated_at. Only inventory models are persisted.
func (s *Service) persistVerdict(ctx context.Context, id string, v compat.Verdict) error {
	const q = `UPDATE models
	SET compat_status = ?, recommended_engine = ?, updated_at = datetime('now')
	WHERE id = ?`
	_, err := s.deps.DB.ExecContext(ctx, q, string(v.Status), nullIfEmpty(v.RecommendedEngine), id)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// quantFromFilename is a tiny heuristic to label the quant of a remote GGUF from its filename
// (e.g. "...-Q4_K_M.gguf" → "q4_k_m", "...-TQ3_4S.gguf" → "tq3_4s"). Used only for routing
// hints (TQ3 detection); the estimator does the real sizing.
func quantFromFilename(file string) string {
	low := strings.ToLower(file)
	for _, q := range []string{"tq3_4s", "tq3_0", "q2_k", "q3_k_m", "q4_k_m", "q4_k_s", "q4_0", "q5_k_m", "q5_0", "q6_k", "q8_0", "iq4_xs", "f16", "bf16"} {
		if strings.Contains(low, q) {
			return q
		}
	}
	return ""
}

// hfBasename returns the repo name (after the last "/") for a display label.
func hfBasename(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 && i+1 < len(repo) {
		return repo[i+1:]
	}
	return repo
}
