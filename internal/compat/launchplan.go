package compat

import (
	"context"
	"fmt"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// LaunchPlan is the concrete, engine-specific plan for actually starting a GGUF model on
// llama.cpp: which binary to spawn, the exact run flags, and the KV type chosen. It is produced
// by PlanLlamaLaunch so the Phase-4 llama.cpp Runner does NOT re-derive binary selection or flag
// building — it reuses the SAME logic the verdict uses (bestLlamaBinary + ggufRunFlags), per the
// task rule "reuse compat's bestLlamaBinary logic via a shared helper, don't fork logic".
type LaunchPlan struct {
	Binary  LlamaBinary // the chosen llama-server binary (Path, Detail, caps)
	Flags   []string    // exact run flags (compat ggufRunFlags: -ngl/-c/-fa/-ctk/-ctv/--mmproj [+ -ncmoe])
	KVType  gguf.KVCacheType
	KVQuant bool
	Reason  string // human-readable why this plan (which binary + why)

	// MoEExpertOffloadMiB is the expert weight (MiB) this plan moves to CPU RAM via `-ncmoe N`
	// (0 when no MoE offload). It is DERIVED from tensor metadata, never guessed — admission may
	// subtract it from the full-GPU footprint to size the reduced GPU residency honestly. When a MoE
	// model opts into offload but its expert bytes are NOT derivable, PlanLlamaLaunch DECLINES (error)
	// rather than emit the flag with an unknown footprint (CLAUDE.md: no speculative admission).
	MoEExpertOffloadMiB int
}

// PlanLlamaLaunch selects the llama-server binary and builds the run flags for a GGUF model at
// the given context, reusing bestLlamaBinary (TQ3 → CUDA fork; else prefer CUDA) and
// ggufRunFlags. kvQuant requests a quantized KV cache when the binary supports it. It returns an
// error (not a verdict) when no suitable binary is available — the caller has already gated on a
// runs/runs_degraded verdict, so this only fails on a genuine "no binary" or "TQ3 needs fork"
// case, which the caller surfaces with the returned reason.
//
// allowCPUOffload opts into a GPU/CPU split (-ngl auto) instead of the full-GPU default (-ngl all).
// It only changes the launch FLAGS; it does NOT relax admission — the caller still gates the start
// on the full-GPU VRAM verdict (docs/api-spec.md instances §allow_cpu_offload).
//
// speculative opts into draft-free self-speculative decoding: "" (SpeculativeOff) = today's flags;
// "ngram-cache" (SpeculativeNgramCache) appends `--spec-type ngram-cache`. The caller has already
// validated it (ValidateSpeculative). It adds ~zero VRAM (a hash pool), so it does NOT relax or
// tighten admission (docs/api-spec.md instances §speculative).
//
// This is the SHARED helper P4.2 uses to avoid forking binary/flag logic out of the rule engine.
func PlanLlamaLaunch(d ModelDescriptor, avail EngineAvailability, ctx int, kvQuant bool, allowCPUOffload bool, speculative string) (LaunchPlan, error) {
	if d.Format != FormatGGUF && d.Format != FormatOllama {
		return LaunchPlan{}, fmt.Errorf("PlanLlamaLaunch: model %q is %s, not GGUF — llama.cpp launch requires a GGUF", d.ID, d.Format)
	}
	requireTQ3 := d.IsTQ3()
	bin, ok := avail.bestLlamaBinary(requireTQ3)
	if !ok {
		if requireTQ3 {
			return LaunchPlan{}, fmt.Errorf("model %q is a TQ3_* quant and needs a has_tq3 llama-server (CUDA fork); only %s is available",
				d.ID, avail.tq3ForkName())
		}
		return LaunchPlan{}, fmt.Errorf("no llama-server binary detected to launch GGUF model %q", d.ID)
	}

	kvType := gguf.KVCacheF16
	kvQuantUsed := false
	if kvQuant && bin.SupportsKVQuant {
		kvType = gguf.KVCacheQ4_0
		kvQuantUsed = true
	}
	flags := ggufRunFlags(d, bin, ctx, kvType, kvQuantUsed, allowCPUOffload, speculative)

	reason := fmt.Sprintf("selected %s (%s) for %s at %s ctx", bin.Path, bin.Detail, d.ID, ctxLabel(ctx))
	if requireTQ3 {
		reason += " — TQ3_* quant requires the has_tq3 CUDA fork"
	}
	if allowCPUOffload {
		reason += " — CPU offload allowed (-ngl auto): llama.cpp offloads as many layers as fit, rest on CPU"
	}
	if speculative == SpeculativeNgramCache {
		reason += " — self-speculative decoding on (--spec-type ngram-cache, draft-free, ~zero extra VRAM)"
	}

	// MoE expert CPU-offload (`-ncmoe N`, research §4). Only when the caller opted into CPU offload:
	// for a MoE GGUF this moves the expert weights of every expert layer to CPU RAM, which is what
	// lets an oversize MoE (e.g. Qwen3.6-35B-A3B) fit on a 16 GB card. Honest sizing (CLAUDE.md, no
	// speculative admission): we emit the flag ONLY when the expert bytes are DERIVABLE from tensor
	// metadata (so admission can subtract them); if a MoE model opts into offload but its expert
	// tensors are not measurable, we DECLINE with a clear reason instead of guessing.
	offloadMiB := 0
	if allowCPUOffload && (d.Format == FormatGGUF || d.Format == FormatOllama) {
		// InspectMoE reads the GGUF header + tensor-info section (no model load). A parse ERROR
		// (notably TQ3_* quants, which gguf-parser-go cannot decode) means we cannot even determine
		// MoE-ness or expert bytes — we then fall back to the plain -ngl auto offload (conservative
		// full-footprint admission), documented in KNOWN-LIMITATIONS. A successful parse routes to the
		// honest-sizing decision below.
		if moe, merr := gguf.InspectMoE(context.Background(), d.GGUFSource); merr == nil {
			mFlags, mOffloadMiB, mReason, mErr := moeOffloadDecision(moe, d.ID)
			if mErr != nil {
				return LaunchPlan{}, mErr
			}
			flags = append(flags, mFlags...)
			offloadMiB = mOffloadMiB
			reason += mReason
		}
	}
	return LaunchPlan{Binary: bin, Flags: flags, KVType: kvType, KVQuant: kvQuantUsed, Reason: reason, MoEExpertOffloadMiB: offloadMiB}, nil
}

// MoEOffloadSizing returns the expert weight (MiB) that a MoE CPU-offload would move OFF the GPU for
// this model under allow_cpu_offload, so admission can size against the reduced GPU footprint BEFORE
// the (binary-dependent) launch plan is built. It returns:
//   - (offloadMiB>0, nil) when the model is a MoE with DERIVABLE expert bytes → subtract from the footprint;
//   - (0, err) when the model is a MoE that opts into offload but whose expert bytes are NOT derivable
//     → the caller must DECLINE the start (no speculative admission);
//   - (0, nil) for a non-MoE model, when offload is off, when the GGUF cannot be parsed (e.g. TQ3_*),
//     or for a non-GGUF format → admission proceeds on the full footprint unchanged.
//
// It reads only the GGUF header + tensor-info section (no binary, no model load), so it is safe to
// call before binary selection / admission (unlike PlanLlamaLaunch).
func MoEOffloadSizing(d ModelDescriptor, allowCPUOffload bool) (offloadMiB int, err error) {
	if !allowCPUOffload || (d.Format != FormatGGUF && d.Format != FormatOllama) {
		return 0, nil
	}
	moe, merr := gguf.InspectMoE(context.Background(), d.GGUFSource)
	if merr != nil {
		return 0, nil // unparseable (e.g. TQ3_*) — fall back to full-footprint admission
	}
	_, offloadMiB, _, err = moeOffloadDecision(moe, d.ID)
	return offloadMiB, err
}

// moeOffloadDecision is the pure (testable) core of MoE CPU-offload planning under allow_cpu_offload.
// For a MoE model it returns the `-ncmoe N` flags, the expert weight moved to CPU (MiB, for
// admission), and a human reason fragment — OR an error declining the offload when the expert bytes
// are NOT derivable from metadata (CLAUDE.md: no speculative admission; every refusal carries a
// reason). A non-MoE model yields no flags and no error (the caller keeps -ngl auto).
func moeOffloadDecision(moe gguf.MoEInfo, modelID string) (flags []string, offloadMiB int, reason string, err error) {
	if !moe.IsMoE {
		return nil, 0, "", nil
	}
	if !moe.Derivable {
		return nil, 0, "", fmt.Errorf("MoE offload sizing not derivable for this model %q: "+
			"expert tensors could not be measured from metadata, so a reduced GPU footprint cannot be "+
			"computed honestly — refusing rather than guess (retry without allow_cpu_offload to run full-GPU)", modelID)
	}
	// Offload every expert layer to CPU (N = expert-layer count = the max meaningful -ncmoe).
	offloadMiB = moe.ExpertBytesMiB()
	flags = []string{"-ncmoe", fmt.Sprintf("%d", moe.ExpertLayers)}
	reason = fmt.Sprintf(" — MoE experts on CPU: %d layers (~%d MiB expert weight off GPU) — allow_cpu_offload",
		moe.ExpertLayers, offloadMiB)
	return flags, offloadMiB, reason, nil
}

// KVTypeName exposes kvTypeName for callers recording the KV type on an observed peak.
func KVTypeName(t gguf.KVCacheType) string { return kvTypeName(t) }
