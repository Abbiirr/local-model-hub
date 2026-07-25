package manifest

// This file defines the "reproduce this" export (P3.3, spec-digest §7 / docs/model-supply-
// chain.md: "Manifest + instances.launch_flags_json = full reproduction"). A ReproduceBundle
// names exactly what bytes, at what revision, with what launch flags, so a run can be
// recreated. It is composed by the core (which reads the manifest + the models row + any
// instance's launch_flags_json) — this file owns only the shape and the per-kind rendering.

// ReproduceBundle is the JSON returned by GET /api/models/{id}/reproduce. It is self-
// describing across all four manifest kinds: HF (repo+ref+commit+files+hashes), gguf_url
// (url+hash), ollama (name+digest), local_import (path+hash+warning).
type ReproduceBundle struct {
	// ModelID is the inventory model this bundle reproduces.
	ModelID string `json:"model_id"`
	// DisplayName is the human label.
	DisplayName string `json:"display_name,omitempty"`
	// Source is the manifest source kind (hf | gguf_url | ollama | local_import), or
	// "unknown" for a pre-existing discovered model with no manifest.
	Source string `json:"source"`
	// Provenance is a human sentence describing how reproducible this is. For discovered
	// pre-existing models it says "unknown (pre-existing local file)".
	Provenance string `json:"provenance"`

	// HF fields (source=hf).
	Repo   string `json:"repo,omitempty"`
	Ref    string `json:"ref,omitempty"`    // the ref requested (main/tag), if recorded
	Commit string `json:"commit,omitempty"` // the pinned commit SHA (the reproduce key)
	Files  []File `json:"files,omitempty"`  // per-file path + sha256 + size

	// gguf_url fields (source=gguf_url).
	URL string `json:"url,omitempty"`

	// ollama fields (source=ollama).
	OllamaName   string `json:"ollama_name,omitempty"`
	OllamaDigest string `json:"ollama_digest,omitempty"`

	// local_import fields (source=local_import).
	LocalPath   string `json:"local_path,omitempty"`
	HashPending bool   `json:"hash_pending,omitempty"` // sha256 still computing in background

	// Common integrity + license.
	PrimarySHA256 string `json:"primary_sha256,omitempty"`
	License       string `json:"license,omitempty"`
	SizeBytes     int64  `json:"size_bytes,omitempty"`

	// Warnings carried from the manifest (pickle/trust_remote_code/local-import origin).
	Warnings []string `json:"warnings,omitempty"`

	// LaunchFlags is the exact command a real run used, from instances.launch_flags_json
	// (the reproducibility promise: manifest + flags = full reproduction). Empty when the
	// model has never been run under LocalHub. Engine names the engine that ran it.
	Engine      string   `json:"engine,omitempty"`
	LaunchFlags []string `json:"launch_flags,omitempty"`

	// ReproduceHint is a one-line human instruction (e.g. the HF command or the ollama
	// pull command) to recreate the download.
	ReproduceHint string `json:"reproduce_hint,omitempty"`
}

// FromManifest builds the manifest-derived portion of a reproduce bundle. The core fills in
// ModelID/DisplayName and the LaunchFlags/Engine from the DB. Returns a bundle even for a nil
// manifest (a discovered pre-existing model): Source="unknown", Provenance names it.
func FromManifest(m *Manifest) ReproduceBundle {
	if m == nil {
		return ReproduceBundle{
			Source:     "unknown",
			Provenance: "unknown (pre-existing local file) — this model was discovered on disk, not downloaded through LocalHub, so its origin, revision, and upstream identity are unknown.",
		}
	}
	b := ReproduceBundle{
		Source:        string(m.Source),
		License:       m.License,
		Warnings:      m.SecurityWarnings,
		PrimarySHA256: m.PrimaryHash(),
		SizeBytes:     m.TotalSizeBytes(),
	}
	switch m.Source {
	case SourceHF:
		b.Repo = m.Repo
		b.Commit = m.Revision
		b.Files = m.Files
		b.Provenance = "reproducible — Hugging Face repo pinned to an exact commit SHA with per-file sha256 hashes."
		b.ReproduceHint = "download " + m.Repo + " at revision " + m.Revision
	case SourceGGUFURL:
		b.URL = m.URL
		b.Files = m.Files
		b.LocalPath = m.LocalPath
		b.Provenance = "reproducible — single GGUF from a direct URL with a verified sha256."
		if m.URL != "" {
			b.ReproduceHint = "download " + m.URL
		}
	case SourceOllama:
		b.OllamaName = m.Name
		b.OllamaDigest = m.Digest
		b.Provenance = "reproducible via Ollama — pinned by model name + digest (Ollama owns the blob)."
		if m.Name != "" {
			b.ReproduceHint = "ollama pull " + m.Name
		}
	case SourceLocalImport:
		b.LocalPath = m.LocalPath
		b.Files = m.Files
		b.HashPending = m.HashPending
		b.Provenance = "local import — origin unverifiable. Reproducible only as the exact bytes at " + m.LocalPath + " (sha256 recorded); no upstream repo/revision is known."
	default:
		b.Provenance = "unknown source kind: " + string(m.Source)
	}
	return b
}
