package compat

import "fmt"

// Status is the top-level compatibility verdict. The four values are the exact vocabulary
// from spec-digest §4 / docs/compatibility-engine.md — do not add to it.
type Status string

const (
	// StatusRuns: the model fits at the requested context with margin, high confidence.
	StatusRuns Status = "runs"
	// StatusRunsDegraded: the model runs, but with a caveat the user must know — fits only
	// at a reduced context, a low-confidence (fallback) estimate near the boundary, requires
	// KV-cache quantization to fit, or a vLLM docker cold-start warning.
	StatusRunsDegraded Status = "runs_degraded"
	// StatusWontRun: the model does not fit on this hardware/engine as configured.
	StatusWontRun Status = "wont_run"
	// StatusUnknown: the estimator could not produce a number at all (e.g. a remote
	// unparseable model, or a total estimator error) — the Reason says what to do next.
	StatusUnknown Status = "unknown"
)

// Confidence mirrors the estimators' confidence vocabulary, normalized for the verdict. It
// is the LEAST confident of the estimator confidences that fed the verdict.
type Confidence string

const (
	ConfidenceExact       Confidence = "exact"       // measured / authoritative
	ConfidenceApproximate Confidence = "approximate" // parsed/computed from real metadata
	ConfidenceFallback    Confidence = "fallback"    // estimator could not parse; heuristic used
)

// AlternativeVerdict is a per-engine breakdown entry: the verdict this specific engine would
// produce for the model. The top-level Verdict picks the best/recommended engine; EngineChoices
// carries the alternatives so the UI can show "runs on llama.cpp (CUDA fork), won't run on
// vLLM" side by side. Each carries its own Reason (mandatory).
type AlternativeVerdict struct {
	Engine             string     `json:"engine"`            // llamacpp | ollama | vllm (+ binary hint in Detail)
	Detail             string     `json:"detail,omitempty"`  // e.g. "CUDA fork (has_tq3)" | "winget (Vulkan/CPU)"
	Status             Status     `json:"status"`            // this engine's verdict
	EstimatedVRAMMiB   int        `json:"estimated_vram_mb"` // required VRAM per this engine's estimate
	MaxContextThatFits int        `json:"max_context_that_fits,omitempty"`
	RunFlags           []string   `json:"run_flags,omitempty"` // exact flags for this engine
	Confidence         Confidence `json:"confidence"`
	Reason             string     `json:"reason"` // MANDATORY, human-readable
}

// Verdict is the structured compatibility answer for one (model, hardware, engines) triple.
// It is the payload behind GET /api/models/{id}/compat and POST /api/compat/preview
// (spec-digest §3/§4). Field names/JSON keys align with the spec's verdict contract; the
// extra fields (Confidence, EngineChoices, Warnings) are additive and documented.
//
// INVARIANT: Reason is ALWAYS non-empty. Verdicts are constructed via newVerdict (below),
// never as bare struct literals in the rule code, so an empty-Reason verdict is structurally
// impossible (and schema-tested). See constants.go package doc.
type Verdict struct {
	Model              string               `json:"model"`              // display id/name of what was evaluated
	Status             Status               `json:"status"`             // runs | runs_degraded | wont_run | unknown
	Reason             string               `json:"reason"`             // MANDATORY, specific, names the numbers
	RecommendedEngine  string               `json:"recommended_engine"` // "" only for unknown/wont-run-everywhere
	EstimatedVRAMMiB   int                  `json:"estimated_vram_mb"`  // required VRAM (recommended engine)
	MaxContextThatFits int                  `json:"max_context_that_fits,omitempty"`
	RequestedContext   int                  `json:"requested_context,omitempty"`
	RunFlags           []string             `json:"run_flags,omitempty"` // exact launch flags for the recommended engine
	Confidence         Confidence           `json:"confidence"`
	Estimator          string               `json:"estimator,omitempty"`      // "gguf-parser-go@..." | "localhub-safetensors" | fallback marker
	EngineChoices      []AlternativeVerdict `json:"engine_choices,omitempty"` // per-engine breakdown
	Warnings           []string             `json:"warnings,omitempty"`

	// Hardware echoes the free/total VRAM the verdict was computed against, so the caller and
	// UI can show "needs X, only Y usable" without a second probe call.
	VRAMFreeMiB  int `json:"vram_free_mb,omitempty"`
	VRAMTotalMiB int `json:"vram_total_mb,omitempty"`
}

// newVerdict is the ONLY constructor for a Verdict. It enforces the non-empty-Reason
// invariant at the type boundary: an empty reason is a programming error (a verdict path
// that forgot its "why"), so it panics — caught by TestNewVerdictPanicsOnEmptyReason and by
// the fact that no production path can trigger it (every call site supplies a reason). This
// makes the "zero-Reason case impossible" per the task's grep-audit requirement: the rule
// code contains no `Verdict{...}` literals that set Status without going through here.
func newVerdict(model string, status Status, reason string) Verdict {
	if reason == "" {
		panic(fmt.Sprintf("compat: newVerdict called with empty reason for model %q status %q — every verdict MUST carry a human-readable reason (CLAUDE.md correctness bar)", model, status))
	}
	return Verdict{
		Model:      model,
		Status:     status,
		Reason:     reason,
		Confidence: ConfidenceApproximate, // default; callers override
	}
}

// newAlternative builds an AlternativeVerdict, enforcing the same non-empty-Reason invariant.
func newAlternative(engine, detail string, status Status, reason string) AlternativeVerdict {
	if reason == "" {
		panic(fmt.Sprintf("compat: newAlternative called with empty reason for engine %q status %q", engine, status))
	}
	return AlternativeVerdict{
		Engine:     engine,
		Detail:     detail,
		Status:     status,
		Reason:     reason,
		Confidence: ConfidenceApproximate,
	}
}

// addWarning appends a warning to the verdict (nil-safe on the slice).
func (v Verdict) addWarning(w string) Verdict {
	if w != "" {
		v.Warnings = append(v.Warnings, w)
	}
	return v
}
