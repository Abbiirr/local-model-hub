package manifest

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// This file implements the security-warning scanner (spec-digest §7 / ADR-004). It inspects
// a manifest's fileset + the fetched config.json and attaches human-readable warnings. The
// policy is WARN, NEVER BLOCK by default: warnings are informational and stored on the
// manifest. Only strict mode (StrictScan) turns a pickle warning into a refusal, and that is
// opt-in (config security.strict_model_artifact_mode, off by default).
//
// The four warning conditions (docs/model-supply-chain.md "Security warnings" table):
//   1. pickle weights (.bin/.pt/.pth) with no safetensors alternative
//   2. requires trust_remote_code (config.json auto_map / a *.py in the repo)
//   3. no license metadata
//   4. gated/consent repo
// plus the local-import "origin unverifiable" provenance warning (spec-digest §7).

// Warning texts are exported so the UI and tests reference the exact strings.
const (
	WarnPickle = "Contains pickle-format weights (.bin/.pt/.pth) and no safetensors alternative. " +
		"Pickle can execute arbitrary code when the weights are loaded. Prefer a safetensors version of this model."
	WarnTrustRemoteCode = "This model requires trust_remote_code — arbitrary code from the repo runs when the model loads. " +
		"Review the repo's custom code before enabling it."
	WarnNoLicense = "No license detected for this model. Check its usage rights before deploying it."
	WarnGatedRepo = "Gated model — the repo's access terms apply. You may need to accept its license on Hugging Face."
	// WarnLocalImportOrigin is attached to every local_import manifest: LocalHub cannot verify
	// where the bytes came from or that they match any upstream repo (spec-digest §7).
	WarnLocalImportOrigin = "Local import — origin unverifiable. LocalHub did not download this artifact and cannot " +
		"confirm its provenance, revision, or that it matches any upstream repo. The recorded sha256 identifies these exact bytes only."
)

// pickleExts are weight file extensions that use Python pickle (code-execution risk).
var pickleExts = map[string]bool{
	".bin": true, ".pt": true, ".pth": true, ".pkl": true, ".ckpt": true,
}

// ScanInput is what the warning scanner inspects. All fields are optional; the scanner uses
// whatever is available (a bare fileset still yields pickle/license warnings).
type ScanInput struct {
	// Files is the manifest fileset (paths matter; hashes/sizes are ignored here).
	Files []File
	// ConfigJSON is the fetched config.json bytes, when available (HF downloads). Used to
	// detect trust_remote_code via auto_map. nil when not fetched.
	ConfigJSON []byte
	// License is the license string discovered from metadata; "" triggers WarnNoLicense.
	License string
	// Gated is true when the source repo is gated/consent-required.
	Gated bool
	// LocalImport is true for local_import sources (always adds WarnLocalImportOrigin).
	LocalImport bool
}

// Scan produces the ordered, de-duplicated list of security warnings for the input. It never
// blocks — it only reports. The order is stable (pickle, trust_remote_code, license, gated,
// local-import) so the UI and tests are deterministic.
func Scan(in ScanInput) []string {
	var out []string

	hasSafetensors := false
	hasPickleWeights := false
	for _, f := range in.Files {
		if isSafetensorsFile(f.Path) {
			hasSafetensors = true
		}
		if pickleExts[strings.ToLower(filepath.Ext(f.Path))] {
			hasPickleWeights = true
		}
	}
	if hasPickleWeights && !hasSafetensors {
		out = append(out, WarnPickle)
	}

	if requiresTrustRemoteCode(in.ConfigJSON, in.Files) {
		out = append(out, WarnTrustRemoteCode)
	}

	if strings.TrimSpace(in.License) == "" {
		out = append(out, WarnNoLicense)
	}

	if in.Gated {
		out = append(out, WarnGatedRepo)
	}

	if in.LocalImport {
		out = append(out, WarnLocalImportOrigin)
	}

	return out
}

// HasPickleWarning reports whether the given warnings include the pickle warning. Used by the
// strict-mode gate to decide whether to refuse a download.
func HasPickleWarning(warnings []string) bool {
	for _, w := range warnings {
		if w == WarnPickle {
			return true
		}
	}
	return false
}

// requiresTrustRemoteCode detects the trust_remote_code requirement two ways:
//   - config.json has an "auto_map" object (the canonical signal that custom modeling code
//     is loaded from the repo), or an explicit "trust_remote_code": true.
//   - the fileset contains a *.py modeling file (modeling_*.py / configuration_*.py), which
//     HF's from_pretrained will only load with trust_remote_code=True.
func requiresTrustRemoteCode(configJSON []byte, files []File) bool {
	if len(configJSON) > 0 {
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(configJSON, &cfg); err == nil {
			if _, ok := cfg["auto_map"]; ok {
				return true
			}
			if raw, ok := cfg["trust_remote_code"]; ok {
				var b bool
				if json.Unmarshal(raw, &b) == nil && b {
					return true
				}
			}
		}
	}
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f.Path))
		if strings.HasSuffix(base, ".py") &&
			(strings.HasPrefix(base, "modeling_") || strings.HasPrefix(base, "configuration_") || strings.HasPrefix(base, "tokenization_")) {
			return true
		}
	}
	return false
}

func isSafetensorsFile(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".safetensors")
}

func isWeightFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".safetensors" || ext == ".gguf" {
		return true
	}
	return pickleExts[ext]
}

func isIndexFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(base, ".index.json")
}
