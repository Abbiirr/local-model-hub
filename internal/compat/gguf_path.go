package compat

import (
	"context"
	"fmt"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// evaluateGGUF handles the GGUF (and Ollama-format) path. Routing:
//   - GGUF -> llama.cpp (preferred binary: CUDA fork if the model is TQ3_* and a has_tq3
//     binary exists; else any detected binary). An ollama-source model may ALSO route to
//     Ollama (only ollama-source models — spec §4 engine recommendation).
//   - TQ3_* models REQUIRE a has_tq3 binary: if none is available -> wont_run naming the fork.
//   - Never GGUF -> vLLM (ADR-004).
func (e *Engine) evaluateGGUF(ctx context.Context, model string, d ModelDescriptor, reqCtx, batch int, kvQuant bool, free, total int, avail EngineAvailability) Verdict {
	requireTQ3 := d.IsTQ3()

	// --- TQ3 hard gate (checked FIRST, before any estimate). ---
	bin, haveBin := avail.bestLlamaBinary(requireTQ3)
	if requireTQ3 && !haveBin {
		return newVerdict(model, StatusWontRun,
			fmt.Sprintf("model is a TQ3_* quant (%s), which needs the CUDA TQ3 fork of llama-server "+
				"(a binary reporting has_tq3). Only %s is available, and it cannot load TQ3 tensors "+
				"(gguf-parser also cannot parse them). Install/point config at the "+
				"llama.cpp-tq3 CUDA build to run this model.",
				labelQuant(d.Quantization), avail.tq3ForkName())).
			with(func(v *Verdict) {
				v.RequestedContext = reqCtx
				v.VRAMFreeMiB = free
				v.VRAMTotalMiB = total
				v.Confidence = ConfidenceFallback
			})
	}
	if !haveBin {
		return newVerdict(model, StatusWontRun,
			"no llama-server binary was detected on this box, so a GGUF model cannot be launched. "+
				"Install llama.cpp (winget) or point config at a llama-server build, then re-run the preview.").
			with(func(v *Verdict) {
				v.RequestedContext = reqCtx
				v.VRAMFreeMiB = free
				v.VRAMTotalMiB = total
			})
	}

	// KV cache type: default f16; use q4_0 when kvQuant requested AND the binary supports it.
	kvType := gguf.KVCacheF16
	kvQuantUsed := false
	if kvQuant && bin.SupportsKVQuant {
		kvType = gguf.KVCacheQ4_0
		kvQuantUsed = true
	}

	// --- Estimate at the requested context. ---
	estReq, err := e.ggufEstimate(ctx, d.GGUFSource, gguf.Options{
		ContextLen:     reqCtx,
		FlashAttention: bin.HasFlashAttn,
		KVCacheType:    kvType,
		NGL:            -1, // full offload (reference-box policy: GPU-only-or-fail)
		ParallelSeq:    batch,
	})
	if err != nil {
		// The estimator only errors when even the fallback cannot produce a number (a remote
		// unparseable model). Surface as unknown with a specific reason (spec §4).
		return newVerdict(model, StatusUnknown,
			fmt.Sprintf("could not estimate VRAM for this GGUF: %v. "+
				"If this is a remote TQ3_* / custom-quant model, download a local copy so the "+
				"file-size fallback can size it, or record a measured VRAM peak (P2.5).", err)).
			with(func(v *Verdict) {
				v.RequestedContext = reqCtx
				v.VRAMFreeMiB = free
				v.VRAMTotalMiB = total
				v.Confidence = ConfidenceFallback
			})
	}

	conf := normalizeGGUFConfidence(estReq.Confidence)
	usable := usableVRAM(free)
	required := float64(estReq.VRAMMiB)

	// When the estimate is a MEASURED observed peak from the calibration store (Confidence
	// "exact"; friction log #6), the number is authoritative, not a heuristic. Surface that in
	// a note so the verdict reads "observed peak ..." — the estimator's Notes already carry the
	// key/date detail; we carry the first note through to the verdict warnings below.
	calibrated := conf == ConfidenceExact

	// --- Max context that fits: probe candidate contexts (don't over-call remote). ---
	maxCtx := e.ggufMaxContext(ctx, d, bin, kvType, batch, usable, reqCtx, estReq.VRAMMiB)

	// --- Run flags for the chosen binary. ---
	flags := ggufRunFlags(d, bin, reqCtx, kvType, kvQuantUsed)

	// --- Build the recommended (llama.cpp) alternative + decide status. ---
	engineLabel := "llamacpp"

	fits := required <= usable
	var status Status
	var reason string
	warnings := []string(nil)

	switch {
	case fits && conf == ConfidenceFallback && nearBoundary(required, usable):
		// Low-confidence estimate near the boundary -> degraded (task spec).
		status = StatusRunsDegraded
		reason = fmt.Sprintf("estimated ~%d MiB at %s ctx fits in %.0f MiB usable (%d total × %.2f safety − %d in use), "+
			"but this is a FALLBACK estimate (gguf-parser could not parse the quant, so the number is a file-size heuristic) "+
			"and it lands within ±%.0f%% of the fit boundary — treat as degraded until a measured VRAM peak confirms it.",
			estReq.VRAMMiB, ctxLabel(reqCtx), usable, total, SafetyFactor, total-free, DegradedBoundaryFraction*100)
	case fits && reqCtx < ReducedContextThreshold:
		status = StatusRunsDegraded
		reason = fmt.Sprintf("fits at the requested %s ctx (~%d MiB in %.0f MiB usable) but only below %s tokens; "+
			"a reduced context is a degraded experience for long-context use.",
			ctxLabel(reqCtx), estReq.VRAMMiB, usable, ctxLabel(ReducedContextThreshold))
	case fits && kvQuantUsed:
		status = StatusRunsDegraded
		reason = fmt.Sprintf("fits at %s ctx (~%d MiB in %.0f MiB usable) ONLY with a quantized KV cache (-ctk/-ctv %s); "+
			"KV quantization trades a little quality for memory — acceptable but degraded.",
			ctxLabel(reqCtx), estReq.VRAMMiB, usable, kvTypeName(kvType))
	case fits && calibrated:
		// Measured observed peak (calibration store) fits — the tightest, highest-confidence
		// "runs": the number is driver-truth from a real load, not an estimate.
		status = StatusRuns
		reason = fmt.Sprintf("MEASURED VRAM peak %d MiB (%s) fits in %.0f MiB usable (%d total × %.2f safety − %d in use), with headroom. "+
			"This is a real load measurement, not an estimate — highest confidence.",
			estReq.VRAMMiB, calibNote(estReq), usable, total, SafetyFactor, total-free)
	case fits:
		status = StatusRuns
		reason = fmt.Sprintf("GGUF needs ~%d MiB at %s ctx and fits in %.0f MiB usable (%d total × %.2f safety − %d in use), with headroom.",
			estReq.VRAMMiB, ctxLabel(reqCtx), usable, total, SafetyFactor, total-free)
	default:
		// Does not fit at requested ctx. Try to fit at a reduced context (or with KV quant).
		degraded, dReason, dFlags, dCtx := e.tryGGUFDegrade(ctx, d, bin, batch, usable, maxCtx, estReq.VRAMMiB, total, free, kvQuantUsed)
		if degraded {
			status = StatusRunsDegraded
			reason = dReason
			flags = dFlags
			maxCtx = dCtx
		} else {
			status = StatusWontRun
			reason = fmt.Sprintf("needs ~%d MiB at %s ctx but only %.0f MiB usable (%d total × %.2f safety − %d in use); "+
				"it does not fit even at the smallest useful context (%s). Use a smaller quant, free VRAM, or a bigger card.",
				estReq.VRAMMiB, ctxLabel(reqCtx), usable, total, SafetyFactor, total-free, ctxLabel(MinDegradedContext))
			// When the estimate is a FALLBACK (gguf-parser could not parse the quant — e.g. TQ3_*),
			// the number is a conservative file-size heuristic. Say so, and note the levers the
			// estimator is blind to (the fork's tq3_0 KV cache, and a measured peak from P2.5) so
			// the user does not read a heuristic wont_run as a hard verdict.
			if conf == ConfidenceFallback {
				reason += " NOTE: this is a FALLBACK estimate (gguf-parser could not parse the quant, so ~weights come from file size); " +
					"it cannot model the fork's tq3_0 KV cache (-ctv tq3_0), which is smaller than the f16 KV assumed here — " +
					"a measured VRAM peak (recorded on first run) may still fit it."
				if bin.HasTQ3 {
					warnings = append(warnings, "TQ3 fork available: the real tq3_0 KV footprint is smaller than this fallback assumes; "+
						"a measured load may fit where this heuristic says it won't.")
				}
			}
			flags = nil // no runnable flags for a won't-run
		}
	}

	if kvQuantUsed && status == StatusRuns {
		warnings = append(warnings, "KV cache is quantized ("+kvTypeName(kvType)+") to fit — slight quality impact.")
	}

	choice := newAlternative(engineLabel, bin.Detail, status, reason).with(func(a *AlternativeVerdict) {
		a.EstimatedVRAMMiB = estReq.VRAMMiB
		a.MaxContextThatFits = maxCtx
		a.RunFlags = flags
		a.Confidence = conf
	})

	v := newVerdict(model, status, reason).with(func(v *Verdict) {
		v.RecommendedEngine = engineLabel
		v.EstimatedVRAMMiB = estReq.VRAMMiB
		v.MaxContextThatFits = maxCtx
		v.RequestedContext = reqCtx
		v.RunFlags = flags
		v.Confidence = conf
		v.Estimator = estReq.ParserVersion
		v.VRAMFreeMiB = free
		v.VRAMTotalMiB = total
		v.EngineChoices = []AlternativeVerdict{choice}
		v.Warnings = warnings
	})

	// Ollama alternative (informational): only ollama-source models route to Ollama.
	if d.Source == "ollama" && avail.OllamaReachable {
		v.EngineChoices = append(v.EngineChoices, newAlternative("ollama", "shared daemon",
			pickOllamaStatus(status),
			fmt.Sprintf("this model is in the Ollama registry (source=ollama); Ollama can serve it (%s). "+
				"Ollama loads the whole model into VRAM and unloads on its own schedule; the VRAM estimate above applies.",
				string(status))).with(func(a *AlternativeVerdict) {
			a.EstimatedVRAMMiB = estReq.VRAMMiB
			a.Confidence = conf
		}))
	}

	return v
}

// pickOllamaStatus mirrors the llama.cpp status for the Ollama alternative (same VRAM math).
func pickOllamaStatus(s Status) Status { return s }

// ggufMaxContext finds the largest candidate context (from ctxCandidates) whose estimate
// fits in usable VRAM. It probes at most len(ctxCandidates) contexts to avoid over-calling a
// remote source (task spec: "3-4 candidate ctx points — don't over-call remote"). It reuses
// the already-computed requested-ctx estimate when a candidate equals reqCtx.
func (e *Engine) ggufMaxContext(ctx context.Context, d ModelDescriptor, bin LlamaBinary, kvType gguf.KVCacheType, batch int, usable float64, reqCtx, reqVRAM int) int {
	best := 0
	for _, c := range ctxCandidates {
		var vram int
		if c == reqCtx {
			vram = reqVRAM
		} else {
			est, err := e.ggufEstimate(ctx, d.GGUFSource, gguf.Options{
				ContextLen:     c,
				FlashAttention: bin.HasFlashAttn,
				KVCacheType:    kvType,
				NGL:            -1,
				ParallelSeq:    batch,
			})
			if err != nil {
				continue
			}
			vram = est.VRAMMiB
		}
		if float64(vram) <= usable {
			if c > best {
				best = c
			}
		}
	}
	return best
}

// tryGGUFDegrade attempts to salvage a wont-fit into runs_degraded by (a) using the max
// context that fits if it is >= MinDegradedContext, or (b) enabling KV quantization if not
// already used and the binary supports it. Returns (degraded, reason, flags, maxCtx).
func (e *Engine) tryGGUFDegrade(ctx context.Context, d ModelDescriptor, bin LlamaBinary, batch int, usable float64, maxCtx, reqVRAM, total, free int, kvQuantUsed bool) (bool, string, []string, int) {
	// (a) reduced context.
	if maxCtx >= MinDegradedContext {
		flags := ggufRunFlags(d, bin, maxCtx, gguf.KVCacheF16, false)
		reason := fmt.Sprintf("does not fit at the requested context in %.0f MiB usable (%d total × %.2f safety − %d in use), "+
			"but fits at a reduced %s ctx — runs degraded at the smaller context.",
			usable, total, SafetyFactor, total-free, ctxLabel(maxCtx))
		return true, reason, flags, maxCtx
	}
	// (b) KV quantization (if supported and not already tried).
	if !kvQuantUsed && bin.SupportsKVQuant {
		est, err := e.ggufEstimate(ctx, d.GGUFSource, gguf.Options{
			ContextLen:     defaultRequestedContext,
			FlashAttention: bin.HasFlashAttn,
			KVCacheType:    gguf.KVCacheQ4_0,
			NGL:            -1,
			ParallelSeq:    batch,
		})
		if err == nil && float64(est.VRAMMiB) <= usable {
			flags := ggufRunFlags(d, bin, defaultRequestedContext, gguf.KVCacheQ4_0, true)
			reason := fmt.Sprintf("does not fit with an f16 KV cache, but fits at %s ctx with a quantized KV cache "+
				"(-ctk/-ctv q4_0, ~%d MiB in %.0f MiB usable) — runs degraded (KV quantization trades a little quality for memory).",
				ctxLabel(defaultRequestedContext), est.VRAMMiB, usable)
			return true, reason, flags, defaultRequestedContext
		}
	}
	return false, "", nil, maxCtx
}

// calibNote returns a compact label for a calibrated (observed-peak) estimate, for the
// verdict reason. The estimator's ParserVersion is "calibration/observed-peak" on the exact
// path; its first Note carries the key/detail. We keep the reason short and let the fuller
// note ride in Warnings (added by the caller). The richer "observed peak from <date> at ctx N"
// phrasing is composed by the core layer (CompatPreview), which has the observed_peaks row.
func calibNote(est gguf.Result) string {
	if len(est.Notes) > 0 {
		return est.Notes[0]
	}
	return "measured observed peak from the calibration store"
}

// normalizeGGUFConfidence maps the gguf package's confidence to the verdict confidence.
func normalizeGGUFConfidence(c gguf.Confidence) Confidence {
	switch c {
	case gguf.ConfidenceExact:
		return ConfidenceExact
	case gguf.ConfidenceApproximate:
		return ConfidenceApproximate
	case gguf.ConfidenceFallback:
		return ConfidenceFallback
	default:
		return ConfidenceApproximate
	}
}

// with runs a mutator on an AlternativeVerdict copy (fluent helper).
func (a AlternativeVerdict) with(f func(*AlternativeVerdict)) AlternativeVerdict {
	f(&a)
	return a
}
