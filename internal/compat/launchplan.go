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
	Binary LlamaBinary // the chosen llama-server binary (Path, Detail, caps)
	Flags  []string    // exact run flags (compat ggufRunFlags: -ngl/-c/-fa/-ctk/-ctv/-np/--spec-type/--mmproj [+ -ncmoe])
	// KVTypeK/KVTypeV are the effective independent cache types (f16 when unset/unsupported).
	KVTypeK gguf.KVCacheType
	KVTypeV gguf.KVCacheType
	KVType  gguf.KVCacheType // kept = KVTypeK for back-compat (calib record key)
	KVQuant bool             // either side non-f16
	Reason  string           // human-readable why this plan (which binary + why)
	Profile LaunchProfile    // the normalized profile that produced this plan (reproducibility, M2.2)

	// MoEExpertOffloadMiB is the expert weight (MiB) this plan moves to CPU RAM via `-ncmoe N`
	// (0 when no MoE offload). It is DERIVED from tensor metadata, never guessed — admission may
	// subtract it from the full-GPU footprint to size the reduced GPU residency honestly. When a MoE
	// model opts into offload but its expert bytes are NOT derivable, PlanLlamaLaunch DECLINES (error)
	// rather than emit the flag with an unknown footprint (CLAUDE.md: no speculative admission).
	MoEExpertOffloadMiB int
}

// PlanLlamaLaunch selects the llama-server binary and builds the run flags for a GGUF model at
// the profile's context, reusing bestLlamaBinary (capability-requirements + configured order,
// M3.6) and ggufRunFlags (profile-driven, M3.4). It returns an error (not a verdict) when no
// suitable binary is available or a requested capability cannot be satisfied — the caller has
// already gated on a runs/runs_degraded verdict, so this surfaces genuine "no binary" /
// "TQ3 needs fork" / "draft-mtp needs model+binary agreement" cases with a reason.
//
// The profile (M2.x) carries independent K/V types, spec mode (incl. draft-mtp), MTP draft max,
// parallel slots, and CPU-offload permission. draft-mtp is gated on BOTH the model embedding an
// MTP head (M3.2) AND the selected binary reporting has_spec_mtp (M3.3); it forces -np 1.
func PlanLlamaLaunch(d ModelDescriptor, avail EngineAvailability, profile LaunchProfile) (LaunchPlan, error) {
	if d.Format != FormatGGUF && d.Format != FormatOllama {
		return LaunchPlan{}, fmt.Errorf("PlanLlamaLaunch: model %q is %s, not GGUF — llama.cpp launch requires a GGUF", d.ID, d.Format)
	}
	profile = profile.Normalize()
	reqs := requirementsForProfile(d, profile)
	bin, ok := avail.bestLlamaBinary(reqs)
	if !ok {
		if reqs.TQ3 {
			return LaunchPlan{}, fmt.Errorf("model %q is a TQ3_* quant and needs a has_tq3 llama-server (CUDA fork); only %s is available",
				d.ID, avail.tq3ForkName())
		}
		if reqs.SpecMTP {
			return LaunchPlan{}, fmt.Errorf("the launch profile requests draft-mtp, which needs a has_spec_mtp llama-server binary "+
				"(one whose --spec-type advertises draft-mtp); none of the detected binaries (%s) support it. "+
				"Use a build with draft-mtp or pick a different speculative mode",
				avail.listBinaryDetails())
		}
		return LaunchPlan{}, fmt.Errorf("no llama-server binary detected to launch GGUF model %q", d.ID)
	}

	// --- MTP model-side gate (M3.3): draft-mtp needs the model to embed an MTP head. ---
	if profile.SpecMode == SpeculativeDraftMTP {
		hasMTP, _, _ := modelMTP(d)
		if !hasMTP {
			return LaunchPlan{}, fmt.Errorf("the launch profile requests draft-mtp (--spec-type draft-mtp), but model %q does not "+
				"embed an MTP / next-n draft head (no %q tensors in the GGUF). Use a GGUF built with an embedded MTP head, "+
				"or pick a different speculative mode (\"\" or \"ngram-cache\")", d.ID, ".nextn.")
		}
	}

	// K/V from the profile (independent), gated on the binary supporting KV quantization.
	kvK, kvV := profile.EffectiveKVTypeK(), profile.EffectiveKVTypeV()
	if !bin.SupportsKVQuant {
		kvK, kvV = gguf.KVCacheF16, gguf.KVCacheF16
	}
	profile.KVTypeK, profile.KVTypeV = kvK, kvV
	kvQuantUsed := profile.KVQuantized()

	flags := ggufRunFlags(d, bin, profile)

	reason := fmt.Sprintf("selected %s (%s) for %s at %s ctx", bin.Path, bin.Detail, d.ID, ctxLabel(profile.Ctx))
	if reqs.TQ3 {
		reason += " — TQ3_* quant requires the has_tq3 CUDA fork"
	}
	if profile.AllowCPUOffload {
		reason += " — CPU offload allowed (-ngl auto): llama.cpp offloads as many layers as fit, rest on CPU"
	}
	switch profile.SpecMode {
	case SpeculativeNgramCache:
		reason += " — self-speculative decoding on (--spec-type ngram-cache, draft-free, ~zero extra VRAM)"
	case SpeculativeDraftMTP:
		reason += fmt.Sprintf(" — embedded MTP head on (--spec-type draft-mtp, --spec-draft-n-max %d, -np 1: the pinned-build acceptance conditions)", profile.SpecDraftMax)
	}
	if kvQuantUsed {
		reason += fmt.Sprintf(" — quantized KV (-ctk %s -ctv %s)", kvTypeName(kvK), kvTypeName(kvV))
	}

	// MoE expert CPU-offload (`-ncmoe N`, research §4). Only when the caller opted into CPU offload:
	// for a MoE GGUF this moves the expert weights of every expert layer to CPU RAM, which is what
	// lets an oversize MoE (e.g. Qwen3.6-35B-A3B) fit on a 16 GB card. Honest sizing (CLAUDE.md, no
	// speculative admission): we emit the flag ONLY when the expert bytes are DERIVABLE from tensor
	// metadata (so admission can subtract them); if a MoE model opts into offload but its expert
	// tensors are not measurable, we DECLINE with a clear reason instead of guessing.
	offloadMiB := 0
	if profile.AllowCPUOffload && (d.Format == FormatGGUF || d.Format == FormatOllama) {
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
	// KVType is the CANONICAL (K,V) key — the value the observed-peak calibration store is indexed
	// by. It MUST NOT be just kvK: a peak measured with `-ctk q8_0 -ctv q4_0` recorded under the
	// bare key "q8_0" would later be matched for a q8_0/q8_0 lookup, letting a measurement taken
	// with a cheap value cache justify an expensive one. Caught LIVE on 2026-07-25, when a real
	// q8_0/q4_0 MTP run recorded `kv_type='q8_0'` in observed_peaks (KNOWN-LIMITATIONS #20).
	// A symmetric pair still renders as the bare type, so pre-existing rows keep matching.
	return LaunchPlan{Binary: bin, Flags: flags, KVTypeK: kvK, KVTypeV: kvV,
		KVType: gguf.CanonicalKVKey(kvK, kvV), KVQuant: kvQuantUsed, Reason: reason,
		Profile: profile, MoEExpertOffloadMiB: offloadMiB}, nil
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
