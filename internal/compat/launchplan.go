package compat

import (
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
	Flags   []string    // exact run flags (compat ggufRunFlags: -ngl/-c/-fa/-ctk/-ctv/--mmproj)
	KVType  gguf.KVCacheType
	KVQuant bool
	Reason  string // human-readable why this plan (which binary + why)
}

// PlanLlamaLaunch selects the llama-server binary and builds the run flags for a GGUF model at
// the given context, reusing bestLlamaBinary (TQ3 → CUDA fork; else prefer CUDA) and
// ggufRunFlags. kvQuant requests a quantized KV cache when the binary supports it. It returns an
// error (not a verdict) when no suitable binary is available — the caller has already gated on a
// runs/runs_degraded verdict, so this only fails on a genuine "no binary" or "TQ3 needs fork"
// case, which the caller surfaces with the returned reason.
//
// This is the SHARED helper P4.2 uses to avoid forking binary/flag logic out of the rule engine.
func PlanLlamaLaunch(d ModelDescriptor, avail EngineAvailability, ctx int, kvQuant bool) (LaunchPlan, error) {
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
	flags := ggufRunFlags(d, bin, ctx, kvType, kvQuantUsed)

	reason := fmt.Sprintf("selected %s (%s) for %s at %s ctx", bin.Path, bin.Detail, d.ID, ctxLabel(ctx))
	if requireTQ3 {
		reason += " — TQ3_* quant requires the has_tq3 CUDA fork"
	}
	return LaunchPlan{Binary: bin, Flags: flags, KVType: kvType, KVQuant: kvQuantUsed, Reason: reason}, nil
}

// KVTypeName exposes kvTypeName for callers recording the KV type on an observed peak.
func KVTypeName(t gguf.KVCacheType) string { return kvTypeName(t) }
