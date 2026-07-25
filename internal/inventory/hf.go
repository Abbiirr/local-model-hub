package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HuggingFace / safetensors model-directory discovery.
//
// A directory is treated as a model root when it contains config.json AND at least one of:
//   - any *.safetensors file, or
//   - model.safetensors.index.json (the sharded-weights index).
//
// This mirrors the standard HF snapshot layout. A directory with config.json but only
// pickle (.bin/.pt) weights is still a pytorch model root (format=pytorch) — handled here
// too, since routing (safetensors vs pickle) and the pickle security warning matter to the
// compat/supply-chain phases. We prefer safetensors (ADR-004) but do not hard-block pickle.

// isHFModelRoot reports whether dir is a model root per the rule above (config.json +
// weights). It only stats entries in dir (no recursion) so it is cheap during the walk.
func isHFModelRoot(dir string) bool {
	if !fileExists(filepath.Join(dir, "config.json")) {
		return false
	}
	return hasWeights(dir) != weightsNone
}

type weightKind int

const (
	weightsNone weightKind = iota
	weightsSafetensors
	weightsPytorch
)

// hasWeights classifies the weight files present directly in dir.
func hasWeights(dir string) weightKind {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return weightsNone
	}
	var sawPickle bool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		switch {
		case strings.HasSuffix(name, ".safetensors"):
			return weightsSafetensors
		case name == "model.safetensors.index.json":
			return weightsSafetensors
		case strings.HasSuffix(name, ".bin"), strings.HasSuffix(name, ".pt"), strings.HasSuffix(name, ".pth"):
			sawPickle = true
		}
	}
	if sawPickle {
		return weightsPytorch
	}
	return weightsNone
}

// hfConfig is the defensive subset of config.json we read. Everything is optional; a
// malformed or partial config yields a warning, not a failure.
type hfConfig struct {
	Architectures []string `json:"architectures"`
	ModelType     string   `json:"model_type"`
	// TorchDtype hints the weight precision (e.g. "bfloat16") used as a quant fallback.
	TorchDtype string `json:"torch_dtype"`
}

// hfModel builds a normalized record for an HF/safetensors model root. It returns ok=false
// only if the directory unexpectedly has no weights (guarded by isHFModelRoot upstream, but
// defended here too). Non-fatal problems (bad config.json) are recorded as warnings on the
// record and errors in res.
func hfModel(root, dir string, res *ScanResult) (Model, bool) {
	kind := hasWeights(dir)
	if kind == weightsNone {
		return Model{}, false
	}

	format := FormatSafetensors
	m := Model{
		ID:          hfNatKey(root, dir),
		DisplayName: filepath.Base(dir),
		Source:      SourceHuggingFace,
		LocalPath:   dir,
	}
	if kind == weightsPytorch {
		format = FormatPyTorch
		m.Warnings = append(m.Warnings,
			"contains pickle-format weights (.bin/.pt/.pth) and no safetensors; pickle can execute code on load — prefer a safetensors version")
	}
	m.Format = format

	// Defensive config.json parse (spec: capture architectures/model_type).
	cfgPath := filepath.Join(dir, "config.json")
	if raw, err := os.ReadFile(cfgPath); err != nil {
		res.Errors = append(res.Errors, err)
		m.Warnings = append(m.Warnings, "config.json present but unreadable: "+err.Error())
	} else {
		var cfg hfConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			m.Warnings = append(m.Warnings, "config.json failed to parse (defensive skip): "+err.Error())
		} else {
			m.Architectures = cfg.Architectures
			m.ModelType = strings.TrimSpace(cfg.ModelType)
			if q := dtypeToQuant(cfg.TorchDtype); q != "" {
				m.Quantization = q
			}
		}
	}

	// Total size = sum of all safetensors shard sizes (the authoritative on-disk size).
	// For pytorch roots, sum the pickle weight files instead.
	if size, mt, ok := sumWeightFiles(dir, kind); ok {
		m.SizeBytes = sizePtr(size)
		m.ModTime = mt
	}

	// Revision: only if real refs metadata exists (a .git dir or an HF refs file). We do
	// NOT invent a revision (spec). Empty stays NULL.
	if rev := detectRevision(dir); rev != "" {
		m.Revision = rev
	}

	// License presence: record the SPDX-ish string if a LICENSE file names one; else leave
	// empty. Cheap heuristic — full license detection is out of scope for the scanner.
	if lic := detectLicense(dir); lic != "" {
		m.License = lic
	}

	return m, true
}

// dtypeToQuant maps a torch_dtype string to the DDL quantization vocabulary. Full-precision
// weights are labeled by their float type (bf16/fp16/fp32); anything else stays unknown.
func dtypeToQuant(dtype string) string {
	switch strings.ToLower(strings.TrimSpace(dtype)) {
	case "bfloat16":
		return "bf16"
	case "float16", "half":
		return "fp16"
	case "float32", "float":
		return "fp32"
	case "float8_e4m3fn", "float8_e5m2", "fp8":
		return "fp8"
	default:
		return ""
	}
}

// sumWeightFiles sums the sizes of the relevant weight files in dir and returns the newest
// mtime among them. kind selects safetensors vs pickle.
func sumWeightFiles(dir string, kind weightKind) (int64, time.Time, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, time.Time{}, false
	}
	var total int64
	var newest time.Time
	var any bool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		match := false
		switch kind {
		case weightsSafetensors:
			match = strings.HasSuffix(name, ".safetensors")
		case weightsPytorch:
			match = strings.HasSuffix(name, ".bin") || strings.HasSuffix(name, ".pt") || strings.HasSuffix(name, ".pth")
		}
		if !match {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		any = true
	}
	return total, newest, any
}

// detectRevision returns an HF commit SHA if the directory carries refs metadata:
//   - a refs/main file (HF snapshot layout) containing a commit sha, or
//   - a .git directory (record "git" as a marker; resolving HEAD is out of scope here).
//
// Returns "" when no revision metadata exists — the scanner never fabricates a revision.
func detectRevision(dir string) string {
	// HF snapshot layout sometimes keeps a refs/ dir with branch->sha files.
	refMain := filepath.Join(dir, "refs", "main")
	if raw, err := os.ReadFile(refMain); err == nil {
		if sha := strings.TrimSpace(string(raw)); sha != "" {
			return sha
		}
	}
	if dirExists(filepath.Join(dir, ".git")) {
		return "git" // marker: a working tree exists; exact HEAD resolution deferred.
	}
	return ""
}

// detectLicense returns a license identifier if a LICENSE/LICENSE.txt file's first line
// looks like an SPDX id or well-known name, else "". Intentionally conservative.
func detectLicense(dir string) string {
	for _, fn := range []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "license"} {
		p := filepath.Join(dir, fn)
		if fileExists(p) {
			// The mere presence is the signal the spec asks for ("license file presence").
			// We return a generic marker rather than mis-parsing full license text.
			return "present"
		}
	}
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// nanoToTime converts a UnixNano int64 (0 = zero time) to a time.Time.
func nanoToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
