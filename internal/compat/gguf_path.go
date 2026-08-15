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
func (e *Engine) evaluateGGUF(ctx context.Context, model string, d ModelDescriptor, reqCtx, batch int, profile LaunchProfile, free, total int, avail EngineAvailability) Verdict {
	profile = profile.Normalize()
	profile.Ctx = reqCtx
	profile.Batch = batch
	reqs := requirementsForProfile(d, profile)

	// --- TQ3 hard gate (checked FIRST, before any estimate). ---
	bin, haveBin := avail.bestLlamaBinary(reqs)
	if reqs.TQ3 && !haveBin {
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

	// Independent K/V cache types from the profile (M2.x/M3.4), gated on the binary supporting
	// KV quantization. A binary without SupportsKVQuant forces f16 for both (the flags/estimate
	// then omit -ctk/-ctv).
	kvK, kvV := profile.EffectiveKVTypeK(), profile.EffectiveKVTypeV()
	if !bin.SupportsKVQuant {
		kvK, kvV = gguf.KVCacheF16, gguf.KVCacheF16
	}
	profile.KVTypeK, profile.KVTypeV = kvK, kvV

	// --- MTP model-side gate (M3.2/M3.3): draft-mtp needs the model to embed an MTP head. ---
	mtpHeadMiB := 0
	if profile.SpecMode == SpeculativeDraftMTP {
		hasMTP, mtpBytes, _ := modelMTP(d)
		if !hasMTP {
			return newVerdict(model, StatusWontRun,
				fmt.Sprintf("the launch profile requests draft-mtp (--spec-type draft-mtp), but model %q does not "+
					"embed an MTP / next-n draft head (no %q tensors found in the GGUF). draft-mtp needs a model "+
					"built with an embedded multi-token-prediction head; use a GGUF that has one, or pick a different "+
					"speculative mode (\"\" or \"ngram-cache\").", d.ID, ".nextn.")).
				with(func(v *Verdict) {
					v.RequestedContext = reqCtx
					v.VRAMFreeMiB = free
					v.VRAMTotalMiB = total
				})
		}
		mtpHeadMiB = mtpBytes
	}

	// --- Estimate at the requested context (independent K/V via the profile, M4.1). ---
	estReq, err := e.ggufEstimate(ctx, d.GGUFSource, gguf.Options{
		ContextLen:     reqCtx,
		FlashAttention: bin.HasFlashAttn,
		KVCacheTypeK:   kvK,
		KVCacheTypeV:   kvV,
		// CalibKey is the CANONICAL key for the (K,V) pair: observed_peaks is indexed by a single
		// kv_type column, so leaving it unset would lose every recorded measurement and drop the
		// verdict from `exact` back to a fallback estimate (the 2026-07-25 regression,
		// KNOWN-LIMITATIONS #20). It is a KEY, not a dtype — deliberately kept OUT of KVCacheType,
		// which the TQ3 fallback and the iSWA floor consume as a real dtype.
		CalibKey:    gguf.CanonicalKVKey(kvK, kvV),
		NGL:         -1, // full offload (reference-box policy: GPU-only-or-fail)
		ParallelSeq: profile.Slots,
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
	// Include the embedded MTP head cost when draft-mtp is on (M4.2): gguf-parser prices the
	// main weights + KV but the nextn head is an honest additive surcharge (~53 MiB on the
	// Qwen3.6 target). Conservative (over-estimate), never a false `runs`.
	if mtpHeadMiB > 0 {
		estReq.VRAMMiB += mtpHeadMiB
		estReq.Notes = append(estReq.Notes, fmt.Sprintf("added %d MiB embedded MTP/nextn head overhead for --spec-type draft-mtp (M4.2)", mtpHeadMiB))
	}

	conf := normalizeGGUFConfidence(estReq.Confidence)
	// A MEASURED peak gets an additive cushion instead of the multiplicative SafetyFactor: the 8%
	// exists to absorb estimator error, and a measurement has none. On a 16 GB card the
	// multiplicative form reserves ~1310 MiB unconditionally, which permanently excludes a model
	// sized to fill the card even with driver-truth numbers in hand. See CalibratedMarginMiB.
	usable := usableVRAMFor(free, conf)
	required := float64(estReq.VRAMMiB)

	// When the estimate is a MEASURED observed peak from the calibration store (Confidence
	// "exact"; friction log #6), the number is authoritative, not a heuristic. Surface that in
	// a note so the verdict reads "observed peak ..." — the estimator's Notes already carry the
	// key/date detail; we carry the first note through to the verdict warnings below.
	calibrated := conf == ConfidenceExact

	// --- Max context that fits: probe candidate contexts (don't over-call remote). ---
	maxCtx := e.ggufMaxContext(ctx, d, bin, kvK, kvV, profile.Slots, mtpHeadMiB, usable, reqCtx, estReq.VRAMMiB)

	// --- Run flags for the chosen binary, from the profile (M2.x/M3.4). ---
	// The verdict advertises the profile's flags (independent K/V, -np, spec mode). For the
	// DEFAULT preview (no profile knobs) this is -ngl all -c CTX --fit off [-fa on] -np 1 — the
	// full-GPU baseline. CPU offload / ngram-cache are profile knobs applied on top at start.
	flags := ggufRunFlags(d, bin, profile)

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
	case fits && profile.KVQuantized():
		status = StatusRunsDegraded
		reason = fmt.Sprintf("fits at %s ctx (~%d MiB in %.0f MiB usable) ONLY with a quantized KV cache "+
			"(-ctk %s -ctv %s); KV quantization trades a little quality for memory — acceptable but degraded.",
			ctxLabel(reqCtx), estReq.VRAMMiB, usable, kvTypeName(kvK), kvTypeName(kvV))
	case fits && !calibrated && withinAdmissionNoise(required, usable):
		// M4.6 — the knife-edge. The estimate fits, but by less than ordinary desktop VRAM churn,
		// so admission is genuinely a coin-flip: the SAME request flipped between 201 and 409 on
		// this box at 8K (margin ~46-61 MiB). Reporting that as a plain `runs` would be a
		// confident answer to an unstable question, so it degrades WITH the numbers and the fix.
		//
		// Ordered AFTER the `calibrated` guard below cannot happen (this case is !calibrated), so a
		// MEASURED peak is never downgraded: a measurement IS the resolution of the knife-edge —
		// on this profile it moves the margin from ~50 MiB to ~600 MiB.
		status = StatusRunsDegraded
		reason = fmt.Sprintf("fits at %s ctx, but only just: ~%d MiB estimated against %.0f MiB usable leaves ~%.0f MiB — "+
			"less than the %d MiB of ordinary VRAM noise on a desktop GPU, so the same request can be admitted one "+
			"minute and refused the next. Start it once to record a measured peak for this exact KV pair (that "+
			"replaces the estimate with driver truth and typically widens the margin substantially), free VRAM, or "+
			"lower the context for a decision that does not depend on noise.",
			ctxLabel(reqCtx), estReq.VRAMMiB, usable, usable-required, AdmissionNoiseMiB)
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
		degraded, dReason, dFlags, dCtx := e.tryGGUFDegrade(ctx, d, bin, profile.Slots, usable, maxCtx, estReq.VRAMMiB, total, free, profile.KVQuantized())
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

	if profile.KVQuantized() && status == StatusRuns {
		warnings = append(warnings, fmt.Sprintf("KV cache is quantized (-ctk %s -ctv %s) — slight quality impact.",
			kvTypeName(kvK), kvTypeName(kvV)))
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

// modelMTP reports whether the descriptor's GGUF embeds an MTP/next-n draft head and its weight
// cost (MiB). It trusts a caller-set ModelDescriptor.HasMTP (manifest cache) for presence and
// otherwise reads the GGUF header + tensor-info (NO model load). A remote/unparseable source
// returns (flag, 0, nil) — the launch path then re-checks / refuses conservatively (M3.2).
func modelMTP(d ModelDescriptor) (bool, int, error) {
	if d.GGUFSource == (gguf.Source{}) {
		return d.HasMTP, 0, nil
	}
	info, err := gguf.InspectMTP(context.Background(), d.GGUFSource)
	if err != nil {
		return d.HasMTP, 0, nil
	}
	return d.HasMTP || info.IsMTP, info.HeadBytesMiB(), nil
}

// ggufMaxContext finds the largest candidate context (from ctxCandidates) whose estimate
// fits in usable VRAM. It probes at most len(ctxCandidates) contexts to avoid over-calling a
// remote source (task spec: "3-4 candidate ctx points — don't over-call remote"). It reuses
// the already-computed requested-ctx estimate when a candidate equals reqCtx. Independent K/V
// types (kvK/kvV), the parallel slot count, and the MTP head overhead are applied consistently
// with the requested-ctx estimate (M4.1/M4.2).
func (e *Engine) ggufMaxContext(ctx context.Context, d ModelDescriptor, bin LlamaBinary, kvK, kvV gguf.KVCacheType, slots, mtpHeadMiB int, usable float64, reqCtx, reqVRAM int) int {
	best := 0
	for _, c := range ctxCandidates {
		var vram int
		if c == reqCtx {
			vram = reqVRAM
		} else {
			est, err := e.ggufEstimate(ctx, d.GGUFSource, gguf.Options{
				ContextLen:     c,
				FlashAttention: bin.HasFlashAttn,
				KVCacheTypeK:   kvK,
				KVCacheTypeV:   kvV,
				CalibKey:       gguf.CanonicalKVKey(kvK, kvV), // calibration KEY, not a dtype
				NGL:            -1,
				ParallelSeq:    slots,
			})
			if err != nil {
				continue
			}
			vram = est.VRAMMiB + mtpHeadMiB
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
// already used and the binary supports it. Returns (degraded, reason, flags, maxCtx). Flags
// are built from LaunchProfiles so the advertised flags match the launch path (M2.x).
func (e *Engine) tryGGUFDegrade(ctx context.Context, d ModelDescriptor, bin LlamaBinary, slots int, usable float64, maxCtx, reqVRAM, total, free int, kvQuantUsed bool) (bool, string, []string, int) {
	// (a) reduced context.
	if maxCtx >= MinDegradedContext {
		p := LaunchProfile{Ctx: maxCtx, Slots: slots}
		flags := ggufRunFlags(d, bin, p)
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
			KVCacheTypeK:   gguf.KVCacheQ4_0,
			KVCacheTypeV:   gguf.KVCacheQ4_0,
			CalibKey:       gguf.CanonicalKVKey(gguf.KVCacheQ4_0, gguf.KVCacheQ4_0), // calibration KEY, not a dtype
			NGL:            -1,
			ParallelSeq:    slots,
		})
		if err == nil && float64(est.VRAMMiB) <= usable {
			p := LaunchProfile{Ctx: defaultRequestedContext, Slots: slots, KVTypeK: gguf.KVCacheQ4_0, KVTypeV: gguf.KVCacheQ4_0}
			flags := ggufRunFlags(d, bin, p)
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
