package compat

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/localhub/localhub/internal/compat/safetensors"
	"github.com/localhub/localhub/internal/hardware"
)

// evaluateSafetensors handles the safetensors (and pytorch) path. Routing: safetensors ->
// vLLM ONLY (never GGUF -> vLLM; ADR-004). Hardware gate FIRST: vLLM requires CC >= 7.5.
// If the CC gate fails, the model wont_run on vLLM (and there is no GGUF here to fall back to).
// cc/ccKnown carry GPU 0's compute capability (from the snapshot) so the vLLM CC gate can be
// enforced — the reference box is 8.9 (passes), but the rule MUST exist (spec §4).
func (e *Engine) evaluateSafetensors(model string, d ModelDescriptor, reqCtx, batch int, kvQuant bool, free, total int, cc float64, ccKnown bool, avail EngineAvailability) Verdict {
	if !d.STConfigResolved || d.STConfig.NumHiddenLayers == 0 {
		return newVerdict(model, StatusUnknown,
			"could not read a usable config.json for this safetensors model (missing num_hidden_layers / dimensions), "+
				"so the VRAM estimate cannot be computed. Ensure config.json is present (local) or reachable (remote HF), then retry.").
			with(func(v *Verdict) {
				v.RequestedContext = reqCtx
				v.VRAMFreeMiB = free
				v.VRAMTotalMiB = total
				v.RecommendedEngine = "vllm"
				v.Confidence = ConfidenceApproximate
			})
	}

	// --- Hardware capability gate (CC >= 7.5), checked FIRST (spec §4). ---
	if ccKnown && cc < vLLMMinComputeCapability {
		return newVerdict(model, StatusWontRun,
			fmt.Sprintf("vLLM requires NVIDIA compute capability >= %.1f, but this GPU is CC %.1f. "+
				"vLLM is the only engine for a safetensors-only model (GGUF->vLLM is not supported, ADR-004); "+
				"convert to GGUF for llama.cpp, or use a newer GPU.", vLLMMinComputeCapability, cc)).
			with(func(v *Verdict) {
				v.RequestedContext = reqCtx
				v.VRAMFreeMiB = free
				v.VRAMTotalMiB = total
				v.RecommendedEngine = "vllm"
				v.Confidence = ConfidenceApproximate
			})
	}

	kvBytes := 2
	if kvQuant {
		kvBytes = 1 // fp8 KV cache
	}

	// --- Estimate at requested context. ---
	est, err := e.stEstimate(d.STConfig, d.STWeights, safetensors.Params{
		Ctx:          reqCtx,
		Batch:        batch,
		KVDtypeBytes: kvBytes,
	})
	if err != nil {
		return newVerdict(model, StatusUnknown,
			fmt.Sprintf("could not estimate VRAM for this safetensors model: %v. "+
				"The config.json is missing dimensions needed to size the KV cache; check num_attention_heads / "+
				"hidden_size / num_hidden_layers.", err)).
			with(func(v *Verdict) {
				v.RequestedContext = reqCtx
				v.VRAMFreeMiB = free
				v.VRAMTotalMiB = total
				v.RecommendedEngine = "vllm"
				v.Confidence = ConfidenceApproximate
			})
	}

	conf := normalizeSTConfidence(est.Confidence)
	usable := usableVRAM(free)
	required := est.TotalMiB

	// vLLM launch guidance (gpu-memory-utilization). Uses TOTAL VRAM (vLLM pre-allocates
	// util * total).
	guidance := est.GPUMemUtil(float64(total))
	maxCtx := e.stMaxCtx(d.STConfig, d.STWeights, float64(free), batch, kvBytes)

	fits := est.FitsInVRAM(float64(free)) // safetensors applies its own 0.92 internally

	dockerCold := strings.EqualFold(avail.VLLMMode, "docker")

	var status Status
	var reason string
	var flags []string
	var warnings []string

	switch {
	case fits && dockerCold:
		status = StatusRunsDegraded
		flags = vllmRunFlags(guidance.RecommendUtil, reqCtx)
		reason = fmt.Sprintf("needs ~%.0f MiB (weights %.0f + KV %.0f + overhead %.0f) at %s ctx and fits in %.0f MiB usable "+
			"(%d total × %.2f safety), but vLLM here runs in DOCKER mode — expect a slow cold start (weight load + CUDA graph capture, tens of seconds). Runs, degraded by cold-start latency.",
			est.TotalMiB, est.WeightsMiB, est.KVCacheMiB, est.OverheadMiB, ctxLabel(reqCtx), usable, total, SafetyFactor)
		warnings = append(warnings, "vLLM docker cold start is slow; the UI must show a STARTING spinner (engine-adapters.md §vLLM).")
	case fits && kvQuant:
		status = StatusRunsDegraded
		flags = vllmRunFlags(guidance.RecommendUtil, reqCtx)
		reason = fmt.Sprintf("needs ~%.0f MiB at %s ctx and fits in %.0f MiB usable (%d total × %.2f safety) ONLY with an fp8 KV cache "+
			"(--kv-cache-dtype fp8); KV quantization trades a little quality for memory — degraded.",
			est.TotalMiB, ctxLabel(reqCtx), usable, total, SafetyFactor)
	case fits && conf == ConfidenceApproximate && nearBoundary(required, usable):
		status = StatusRunsDegraded
		flags = vllmRunFlags(guidance.RecommendUtil, reqCtx)
		reason = fmt.Sprintf("estimated ~%.0f MiB at %s ctx fits in %.0f MiB usable (%d total × %.2f safety), but the weights size is "+
			"APPROXIMATE (no index/shard size — param-count fallback) and lands within ±%.0f%% of the boundary; treat as degraded until confirmed.",
			est.TotalMiB, ctxLabel(reqCtx), usable, total, SafetyFactor, DegradedBoundaryFraction*100)
	case fits && reqCtx < ReducedContextThreshold:
		status = StatusRunsDegraded
		flags = vllmRunFlags(guidance.RecommendUtil, reqCtx)
		reason = fmt.Sprintf("fits at the requested %s ctx (~%.0f MiB in %.0f MiB usable) but only below %s tokens; a reduced context is degraded for long-context use.",
			ctxLabel(reqCtx), est.TotalMiB, usable, ctxLabel(ReducedContextThreshold))
	case fits:
		status = StatusRuns
		flags = vllmRunFlags(guidance.RecommendUtil, reqCtx)
		reason = fmt.Sprintf("safetensors on vLLM needs ~%.0f MiB (weights %.0f + KV %.0f + overhead %.0f) at %s ctx and fits in %.0f MiB usable "+
			"(%d total × %.2f safety); set --gpu-memory-utilization %.2f. GQA=%v.",
			est.TotalMiB, est.WeightsMiB, est.KVCacheMiB, est.OverheadMiB, ctxLabel(reqCtx), usable, total, SafetyFactor, guidance.RecommendUtil, est.IsGQA)
	default:
		// Does not fit at requested ctx. Try a reduced context.
		if maxCtx >= MinDegradedContext {
			status = StatusRunsDegraded
			flags = vllmRunFlags(guidance.RecommendUtil, maxCtx)
			reason = fmt.Sprintf("does not fit at %s ctx (needs ~%.0f MiB, only %.0f MiB usable = %d total × %.2f safety), "+
				"but fits at a reduced %s ctx — runs degraded at the smaller context.",
				ctxLabel(reqCtx), est.TotalMiB, usable, total, SafetyFactor, ctxLabel(maxCtx))
		} else if !guidance.Fits {
			status = StatusWontRun
			reason = fmt.Sprintf("weights+overhead need ~%.0f MiB, which exceeds %.0f%% of the %d MiB device even at max "+
				"gpu-memory-utilization; won't run on vLLM. Use a smaller quant (AWQ/GPTQ), tensor-parallel across GPUs, or a GGUF via llama.cpp.",
				est.WeightsMiB+est.OverheadMiB, vLLMMaxGPUMemUtil*100, total)
		} else {
			status = StatusWontRun
			reason = fmt.Sprintf("needs ~%.0f MiB at %s ctx but only %.0f MiB usable (%d total × %.2f safety − %d in use), and it does not fit "+
				"even at the smallest useful context (%s). Use a smaller quant or free VRAM.",
				est.TotalMiB, ctxLabel(reqCtx), usable, total, SafetyFactor, total-free, ctxLabel(MinDegradedContext))
		}
	}

	// vLLM availability caveat: if vLLM is not actually usable on this box, note it (the
	// verdict is still valid — it says whether the model WOULD run on vLLM).
	if !avail.VLLMAvailable {
		warnings = append(warnings, "vLLM is not currently reachable/installed on this box (no native Windows support); "+
			"this verdict is the vLLM fit estimate — provision vLLM (docker/attach) to actually run it.")
	}

	choice := newAlternative("vllm", vllmDetail(avail), status, reason).with(func(a *AlternativeVerdict) {
		a.EstimatedVRAMMiB = roundMiB(est.TotalMiB)
		a.MaxContextThatFits = maxCtx
		a.RunFlags = flags
		a.Confidence = conf
	})

	return newVerdict(model, status, reason).with(func(v *Verdict) {
		v.RecommendedEngine = "vllm"
		v.EstimatedVRAMMiB = roundMiB(est.TotalMiB)
		v.MaxContextThatFits = maxCtx
		v.RequestedContext = reqCtx
		v.RunFlags = flags
		v.Confidence = conf
		v.Estimator = "localhub-safetensors"
		v.VRAMFreeMiB = free
		v.VRAMTotalMiB = total
		v.EngineChoices = []AlternativeVerdict{choice}
		v.Warnings = warnings
	})
}

// vllmDetail describes the vLLM launch mode for the alternative label.
func vllmDetail(avail EngineAvailability) string {
	if avail.VLLMMode == "" {
		return "vLLM (mode unset)"
	}
	return "vLLM (" + avail.VLLMMode + " mode)"
}

// normalizeSTConfidence maps safetensors confidence to the verdict confidence.
func normalizeSTConfidence(c safetensors.Confidence) Confidence {
	switch c {
	case safetensors.ConfidenceExact:
		return ConfidenceExact
	case safetensors.ConfidenceApproximate:
		return ConfidenceApproximate
	default:
		return ConfidenceApproximate
	}
}

// parseCC parses a compute-capability string like "8.9" into a float. Returns (0,false) on
// an unparseable/empty value.
func parseCC(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// ccFromSnapshot reads GPU 0's compute capability from a snapshot.
func ccFromSnapshot(snap hardware.Snapshot) (float64, bool) {
	if len(snap.GPUs) == 0 {
		return 0, false
	}
	return parseCC(snap.GPUs[0].ComputeCapability)
}
