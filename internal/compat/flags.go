package compat

import (
	"fmt"
	"strings"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// Spec-mode constants and ValidateSpeculative live in profile.go (LaunchProfile). This file
// holds the low-level llama-server flag builder shared by the verdict path (evaluateGGUF) and
// the launch path (PlanLlamaLaunch).

// ggufRunFlags builds the exact llama-server launch flags for a GGUF verdict/launch from a
// LaunchProfile (M2.x/M3.4). Emitted shape (the validated Qwen3.6-27B MTP profile):
//
//	-ngl all|auto -c <CTX> --fit off [-fa on] [-ctk <K> -ctv <V>] [-np <SLOTS>]
//	[--embedding [--pooling none|mean|cls|last|rank] [--reranking]]
//	[--spec-type ngram-cache|draft-mtp [--spec-draft-n-max N]] [--mmproj <path>]
//
// Independent K/V (M3.4): -ctk/-ctv come from the PROFILE (KVTypeK/KVTypeV), NOT from a
// capability bit. The old capability-driven `-ctv tq3_0` (when bin.HasTQ3) is gone — replaced,
// not added alongside (TODO M3.4). -fa is gated on the binary reporting flash-attn; -ctk/-ctv
// only when either side is non-f16; -np always emitted (default 1); --spec-type/--spec-draft-n-max
// only when the profile opts into a spec mode (draft-mtp also requires model+binary agreement,
// checked by PlanLlamaLaunch before calling here).
//
// -ngl semantics ([RE-VERIFY 2026-07-07] against the LOCAL binaries' --help): 'all' (full GPU,
// the hard default) or 'auto' (allow_cpu_offload, GPU/CPU split). Admission is NOT relaxed by
// 'auto' (see PlanLlamaLaunch / core StartInstance).
func ggufRunFlags(d ModelDescriptor, bin LlamaBinary, p LaunchProfile) []string {
	p = p.Normalize()
	ngl := "all"
	if p.AllowCPUOffload {
		ngl = "auto"
	}
	flags := []string{"-ngl", ngl, "-c", fmt.Sprintf("%d", p.Ctx), "--fit", "off"}
	if bin.HasFlashAttn {
		flags = append(flags, "-fa", "on")
	}
	// Independent K/V cache types from the profile (replaces the old capability-bit -ctv tq3_0).
	k, v := p.EffectiveKVTypeK(), p.EffectiveKVTypeV()
	if k != gguf.KVCacheF16 || v != gguf.KVCacheF16 {
		if k != gguf.KVCacheF16 {
			flags = append(flags, "-ctk", kvTypeName(k))
		}
		if v != gguf.KVCacheF16 {
			flags = append(flags, "-ctv", kvTypeName(v))
		}
	}
	// Parallel server slots (-np). draft-mtp forces 1 (Normalize); always emitted so the
	// pinned profile's -np 1 is explicit and reproducible.
	flags = append(flags, "-np", fmt.Sprintf("%d", p.Slots))
	// Embedding-only mode. Primary source (llama.cpp tools/server/README.md, re-verified 2026-08-05):
	//   "--embedding, --embeddings : restrict to only support embedding use case; use only with
	//    dedicated embedding models (default: disabled)"
	//   "--pooling {none,mean,cls,last,rank} : pooling type for embeddings, use model default if
	//    unspecified"
	// --pooling is emitted ONLY when the profile names one; omitted means the model's own default,
	// which is what a dedicated embedding GGUF normally wants.
	if p.Embedding {
		flags = append(flags, "--embedding")
		if p.Pooling != "" {
			flags = append(flags, "--pooling", p.Pooling)
		}
		// Reranking. Primary source (llama.cpp tools/server/README.md, re-verified 2026-08-05):
		//   "--rerank, --reranking : enable reranking endpoint on server (default: disabled)"
		//   "Requires a reranker model (such as bge-reranker-v2-m3) and the --embedding --pooling
		//    rank options."
		// Normalize already forces Embedding=true and Pooling=PoolingRank whenever Reranking is
		// true, so by the time we get here p.Pooling is "rank" and the pair above already emitted
		// `--pooling rank`; --reranking is appended AFTER it so the emitted order reads
		// `--embedding --pooling rank --reranking`, matching the doc-commented shape.
		if p.Reranking {
			flags = append(flags, "--reranking")
		}
	}
	// Speculative decoding is meaningless for an encoder-only embedding server (no autoregressive
	// decode to draft ahead of). Normalize already forced SpecMode off in embedding mode; the guard
	// is explicit so a future caller that bypasses Normalize still cannot emit --spec-type here.
	if !p.Embedding {
		switch p.SpecMode {
		case SpeculativeNgramCache:
			flags = append(flags, "--spec-type", SpeculativeNgramCache)
		case SpeculativeDraftMTP:
			flags = append(flags, "--spec-type", SpeculativeDraftMTP)
			// --spec-draft-n-max is probed SEPARATELY from draft-mtp (M3.1): they are distinct tokens
			// in --help and a build may expose one without the other, so we never emit a flag the
			// selected binary did not advertise. Omitted => llama.cpp's own default (3 on b10106).
			if p.SpecDraftMax > 0 && bin.HasSpecDraftMax {
				flags = append(flags, "--spec-draft-n-max", fmt.Sprintf("%d", p.SpecDraftMax))
			}
		}
	}
	if d.HasMMProj && d.MMProjPath != "" {
		flags = append(flags, "--mmproj", d.MMProjPath)
	}
	return flags
}

// vllmRunFlags builds the vLLM launch flags for a runnable safetensors verdict: the
// gpu-memory-utilization from the safetensors RecommendUtil + max-model-len.
func vllmRunFlags(recommendUtil float64, maxModelLen int) []string {
	return []string{
		"--gpu-memory-utilization", fmt.Sprintf("%.2f", recommendUtil),
		"--max-model-len", fmt.Sprintf("%d", maxModelLen),
		"--dtype", "auto",
	}
}

// kvTypeName renders a gguf.KVCacheType for a flag/reason (e.g. "q4_0").
func kvTypeName(t gguf.KVCacheType) string {
	if t == "" {
		return "f16"
	}
	return string(t)
}

// ctxLabel renders a context length compactly (e.g. 8192 -> "8K", 16384 -> "16K", 1500 -> "1500").
func ctxLabel(ctx int) string {
	if ctx <= 0 {
		return "max"
	}
	if ctx%1024 == 0 {
		return fmt.Sprintf("%dK", ctx/1024)
	}
	return fmt.Sprintf("%d", ctx)
}

// labelQuant renders a quantization label for a reason, defaulting to "TQ3" when empty but
// the model was detected as TQ3 by filename.
func labelQuant(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "TQ3_*"
	}
	return strings.ToUpper(q)
}
