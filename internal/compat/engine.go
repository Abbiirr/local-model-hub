package compat

import (
	"context"
	"fmt"
	"math"

	"github.com/localhub/localhub/internal/compat/gguf"
	"github.com/localhub/localhub/internal/compat/safetensors"
	"github.com/localhub/localhub/internal/hardware"
)

// Request bundles everything one Evaluate call needs beyond the descriptor: the target
// context/batch and the live hardware + engine availability.
type Request struct {
	Descriptor   ModelDescriptor
	RequestedCtx int // requested context tokens; 0 => default (8192)
	Batch        int // concurrent sequences; 0 => 1
	// KVQuant, if true, requests a quantized KV cache (q4_0/q8_0 GGUF; fp8 safetensors) to
	// help a near-boundary model fit. The engine may set this itself when a model fits only
	// with KV quant (runs_degraded).
	KVQuant bool
}

// defaultRequestedContext when the caller passes 0. Matches config.Compatibility.DefaultContext.
const defaultRequestedContext = 8192

// Engine is the compat rule engine. It holds no state beyond the injected estimators (via
// function seams for tests) and is safe for concurrent use.
type Engine struct {
	// ggufEstimate is the GGUF estimator seam (defaults to gguf.Estimate, or, when a
	// calibration store is wired, gguf.EstimateWithCalibration bound to it). Tests inject a fake.
	ggufEstimate func(ctx context.Context, src gguf.Source, opts gguf.Options) (gguf.Result, error)
	// stEstimate is the safetensors estimator seam (defaults to safetensors.EstimateVRAM).
	stEstimate func(cfg safetensors.ModelConfig, w safetensors.WeightsInfo, p safetensors.Params) (safetensors.Estimate, error)
	// stMaxCtx is the safetensors max-context seam (defaults to safetensors.MaxContextThatFits).
	stMaxCtx func(cfg safetensors.ModelConfig, w safetensors.WeightsInfo, freeVRAMMiB float64, batch, kvDtypeBytes int) int
}

// New builds an Engine wired to the real estimators, with NO calibration store (estimates
// come from gguf-parser / the fallback heuristic only). Prefer NewWithCalibration once a
// store exists so measured VRAM peaks tighten the verdict (P2.5).
func New() *Engine {
	return NewWithCalibration(nil)
}

// NewWithCalibration builds an Engine whose GGUF estimator prefers a MEASURED observed peak
// (Confidence "exact") over any computed estimate, by consulting the given calibration store
// first (gguf.EstimateWithCalibration). A nil store is equivalent to New(): the estimator
// falls through to parse/fallback. This is how CompatPreview tightens a verdict after a model
// has been really loaded once (the measured-beats-estimate loop; friction log #6).
func NewWithCalibration(store gguf.CalibrationStore) *Engine {
	ggufEst := gguf.Estimate
	if store != nil {
		ggufEst = func(ctx context.Context, src gguf.Source, opts gguf.Options) (gguf.Result, error) {
			return gguf.EstimateWithCalibration(ctx, src, opts, store)
		}
	}
	return &Engine{
		ggufEstimate: ggufEst,
		stEstimate:   safetensors.EstimateVRAM,
		stMaxCtx:     safetensors.MaxContextThatFits,
	}
}

// Evaluate is the single entry point: it routes by format, calls the appropriate estimator(s)
// against the live hardware, applies the rules, and returns a Verdict whose Reason names the
// numbers. It NEVER returns a Verdict with an empty Reason (constructor-enforced). An error is
// returned only for a caller-side problem (nil snapshot); an estimator failure is folded into
// an unknown/wont_run verdict with a reason, never propagated as a Go error (a model must
// always get a verdict — spec §4).
func (e *Engine) Evaluate(ctx context.Context, req Request, snap hardware.Snapshot, avail EngineAvailability) Verdict {
	d := req.Descriptor
	model := d.DisplayName
	if model == "" {
		model = d.ID
	}
	reqCtx := req.RequestedCtx
	if reqCtx <= 0 {
		reqCtx = defaultRequestedContext
	}
	batch := req.Batch
	if batch <= 0 {
		batch = 1
	}

	free, total, gpuOK := gpuVRAM(snap)
	if !gpuOK {
		return newVerdict(model, StatusUnknown,
			"no NVIDIA GPU detected in the live hardware snapshot; cannot estimate GPU VRAM fit. "+
				"Attach a GPU or check the nvidia-smi probe, then re-run the compatibility preview.").
			with(func(v *Verdict) { v.RequestedContext = reqCtx; v.Confidence = ConfidenceApproximate })
	}

	cc, ccKnown := ccFromSnapshot(snap)

	switch d.Format {
	case FormatGGUF, FormatOllama:
		return e.evaluateGGUF(ctx, model, d, reqCtx, batch, req.KVQuant, free, total, avail)
	case FormatSafetensors:
		return e.evaluateSafetensors(model, d, reqCtx, batch, req.KVQuant, free, total, cc, ccKnown, avail)
	case FormatPyTorch:
		// pickle/pytorch: safetensors-path math still applies for size, but route note differs.
		return e.evaluateSafetensors(model, d, reqCtx, batch, req.KVQuant, free, total, cc, ccKnown, avail).
			addWarning("pytorch/pickle weights: prefer a safetensors version (pickle can execute code on load); " +
				"vLLM can serve it but the security warning applies (ADR-004).")
	default:
		return newVerdict(model, StatusUnknown,
			fmt.Sprintf("unrecognized model format %q; cannot route to an engine or estimator. "+
				"Supported: gguf, safetensors, pytorch, ollama.", d.Format)).
			with(func(v *Verdict) { v.RequestedContext = reqCtx; v.VRAMFreeMiB = free; v.VRAMTotalMiB = total })
	}
}

// gpuVRAM extracts (freeMiB, totalMiB, ok) for GPU 0 from the snapshot. ok is false when no
// GPU is present. Free is the LIVE free VRAM (the P0 lesson: desktop eats ~2.8 GB, so total
// is NOT usable — we always size against free).
func gpuVRAM(snap hardware.Snapshot) (free, total int, ok bool) {
	if len(snap.GPUs) == 0 {
		return 0, 0, false
	}
	g := snap.GPUs[0]
	return g.VRAMFreeMiB, g.VRAMTotalMiB, true
}

// usableVRAM applies the supply-side safety_factor to free VRAM.
func usableVRAM(freeMiB int) float64 { return float64(freeMiB) * SafetyFactor }

// with runs a mutator on a copy of the verdict and returns it (fluent helper for setting the
// echo fields without a struct literal that could drop Reason).
func (v Verdict) with(f func(*Verdict)) Verdict {
	f(&v)
	return v
}

// nearBoundary reports whether required is within ±DegradedBoundaryFraction of usable.
func nearBoundary(requiredMiB, usableMiB float64) bool {
	if usableMiB <= 0 {
		return false
	}
	lo := usableMiB * (1 - DegradedBoundaryFraction)
	hi := usableMiB * (1 + DegradedBoundaryFraction)
	return requiredMiB >= lo && requiredMiB <= hi
}

// roundMiB rounds a float MiB figure to an int for the verdict.
func roundMiB(x float64) int { return int(math.Round(x)) }
