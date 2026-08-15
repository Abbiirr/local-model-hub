package compat

import (
	"fmt"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// LaunchProfile is the versioned, behavior-affecting launch profile for a GGUF start on
// llama.cpp — the single contract threaded REST → core → PlanLlamaLaunch → UI → queue (migration
// 0009) → jobs → instance record (M2.1/M2.2). Every field changes the emitted flags and/or the
// VRAM estimate, so the WHOLE profile MUST survive queue replay byte-identically (M2.5): a
// queued start replays to the same normalized plan across a daemon restart, never silently
// regressing to f16 KV (the 0006 defect).
//
// Independent K/V (KVTypeK/KVTypeV) replaces the bare kv_quant bool: q8_0/q4_0 is now
// expressible end-to-end. SpecMode "draft-mtp" enables the model's EMBEDDED MTP head via
// `--spec-type draft-mtp` (gated on model HasMTP AND binary has_spec_mtp — M3.3); it forces a
// single server slot (-np 1), the pinned-build acceptance condition. Flash attention is derived
// from the selected binary's capability (auto), not a profile field, in v1.
type LaunchProfile struct {
	Ctx   int
	Batch int // concurrent sequences for KV sizing (estimator ParallelSeq); 0 => 1
	Slots int // -np (llama.cpp parallel server slots); 0 => 1. draft-mtp forces 1.
	// KVTypeK / KVTypeV are the INDEPENDENT key/value cache dtypes. Empty => f16.
	KVTypeK gguf.KVCacheType
	KVTypeV gguf.KVCacheType
	// SpecMode selects speculative decoding: "" (off), "ngram-cache" (draft-free self-spec),
	// or "draft-mtp" (embedded MTP head; needs model+binary agreement). Validated by
	// ValidateSpeculative.
	SpecMode string
	// SpecDraftMax is `--spec-draft-n-max N` for draft-mtp. 0 => omit the flag (llama.cpp
	// default). The pinned Qwen3.6 profile uses 2.
	SpecDraftMax int
	// AllowCPUOffload opts into a GPU/CPU split (-ngl auto) instead of the full-GPU default
	// (-ngl all). Changes FLAGS only; admission is NOT relaxed.
	AllowCPUOffload bool
	// Embedding starts the server in embedding-ONLY mode (`--embedding`). llama.cpp documents the
	// flag as "restrict to only support embedding use case; use only with dedicated embedding
	// models" (tools/server/README.md), so this is not a superset of a generation start: the SAME
	// process cannot serve both /v1/chat/completions and /v1/embeddings. That is why the mode is an
	// explicit profile field rather than something inferred from the model — it decides which
	// routes the spawned process can answer, and the proxy must never send an embeddings request to
	// a server started without it. Changes FLAGS only; admission is NOT relaxed (the KV/weight
	// footprint is estimated exactly as for a generation start).
	Embedding bool
	// Pooling is llama.cpp's `--pooling {none,mean,cls,last,rank}` — how token vectors are reduced
	// to one embedding per input. "" omits the flag entirely, which is what llama.cpp calls "use
	// model default" and is the right choice for a dedicated embedding GGUF that declares its own.
	// It matters here because the OpenAI-compatible /v1/embeddings route "requires that the model
	// uses a pooling different than type none" (tools/server/README.md): a model whose default is
	// none can only be served through llama.cpp's native /embedding route unless this field
	// overrides it. Meaningful only when Embedding is true.
	Pooling string
	// Reranking enables llama.cpp's rerank endpoints (`--reranking`). Primary source
	// (tools/server/README.md, master, fetched 2026-08-05):
	//   "--rerank, --reranking : enable reranking endpoint on server (default: disabled)"
	//   "Requires a reranker model (such as bge-reranker-v2-m3) and the --embedding --pooling
	//    rank options."
	// So reranking is NOT a separate llama.cpp server mode alongside embedding — it is embedding
	// mode PLUS --pooling rank PLUS --reranking, all three required together. Setting this field
	// therefore IMPLIES Embedding=true and Pooling=PoolingRank (see Normalize); it is still its
	// own field, not folded into Embedding+Pooling==rank, because a reranker answers a distinct
	// route with a distinct output shape: it returns a relevance SCORE per (query, document) pair,
	// not a reusable embedding vector, so LocalHub must route /v1/rerank only to an instance
	// started this way and never let it warm-route a plain /v1/embeddings request (see
	// core.launchModeOfFlags).
	Reranking bool
	Pin       bool
	Priority  int
}

// SpecMode constants. The `speculative` API field keeps its name for back-compat; these are its
// accepted values. draft-mtp is the embedded multi-token-prediction head.
const (
	SpeculativeOff        = ""
	SpeculativeNgramCache = "ngram-cache"
	SpeculativeDraftMTP   = "draft-mtp"
)

// ValidateSpeculative validates a per-start speculative/spec-mode value. "" (off),
// "ngram-cache" (draft-free self-spec), and "draft-mtp" (embedded MTP head) are accepted;
// anything else is rejected with a reason listing the valid values (CLAUDE.md: every refusal
// carries a human-readable reason). draft-mtp is additionally gated on model+binary agreement
// in PlanLlamaLaunch (M3.3). Returns the normalized value.
func ValidateSpeculative(s string) (string, error) {
	switch s {
	case SpeculativeOff, SpeculativeNgramCache, SpeculativeDraftMTP:
		return s, nil
	default:
		return "", fmt.Errorf("invalid speculative value %q: valid values are %q (off), %q (draft-free self-speculative, --spec-type ngram-cache), or %q (embedded MTP head, --spec-type draft-mtp; needs a model with MTP tensors and a has_spec_mtp binary)",
			s, SpeculativeOff, SpeculativeNgramCache, SpeculativeDraftMTP)
	}
}

// Pooling constants for llama.cpp's `--pooling {none,mean,cls,last,rank}` (LaunchProfile.Pooling).
// PoolingDefault ("") omits the flag entirely — llama.cpp then uses the model's own default.
const (
	PoolingDefault = "" // omit --pooling; use the model's own default
	PoolingNone    = "none"
	PoolingMean    = "mean"
	PoolingCLS     = "cls"
	PoolingLast    = "last"
	PoolingRank    = "rank"
)

// ValidatePooling validates a per-start `--pooling` value. "" (use the model default), "none",
// "mean", "cls", "last", and "rank" are accepted (tools/server/README.md: "--pooling
// {none,mean,cls,last,rank} : pooling type for embeddings, use model default if unspecified");
// anything else is rejected with a reason listing the valid set AND noting that "" means "use the
// model default" (CLAUDE.md: every refusal carries a human-readable reason).
//
// WARNING baked into this doc comment, not just the refusal path: accepting "none" is valid
// input, but a server started with pooling "none" cannot answer llama.cpp's OpenAI-compatible
// /v1/embeddings route — tools/server/README.md says that route "requires that the model uses a
// pooling different than type none". The server's own native /embedding route still works fine
// with pooling none; only LocalHub's OpenAI-compatible /v1/embeddings proxy will fail against
// such a server. Returns the normalized value.
func ValidatePooling(s string) (string, error) {
	switch s {
	case PoolingDefault, PoolingNone, PoolingMean, PoolingCLS, PoolingLast, PoolingRank:
		return s, nil
	default:
		return "", fmt.Errorf("invalid pooling value %q: valid values are \"\" (use the model default), %q, %q, %q, %q, or %q",
			s, PoolingNone, PoolingMean, PoolingCLS, PoolingLast, PoolingRank)
	}
}

// MaxSlots caps the requestable `-np` server-slot count. This box is a single 16 GB card and each
// slot multiplies the KV cache linearly (the estimator's ParallelSeq), so a large value is far
// more likely a typo than an intent. A rejected value carries the bound in its reason.
const MaxSlots = 32

// MaxSpecDraftMax caps `--spec-draft-n-max`. The validated Qwen3.6 profile uses 2; b10106's own
// default is 3. Past a handful of draft tokens the acceptance rate collapses and the extra compute
// is wasted, so we bound it rather than forward an arbitrary number to the engine.
const MaxSpecDraftMax = 16

// ValidateKVCacheType validates ONE side of the KV cache dtype (`-ctk` or `-ctv`). Accepted:
// "" (unset => f16, the llama.cpp default), "f16", "q8_0", "q4_0". side is "k" or "v" and is used
// only to make the refusal reason specific.
//
// tq3_0 is deliberately NOT accepted even though the CUDA fork advertises it: the estimator
// (gguf.kvGGMLType) cannot price it, so admitting it would mean spawning flags whose VRAM cost we
// cannot compute — a speculative admission, which CLAUDE.md forbids. Every refusal names the valid
// set (CLAUDE.md: a human-readable reason on every no).
func ValidateKVCacheType(s, side string) (gguf.KVCacheType, error) {
	switch gguf.KVCacheType(s) {
	case "":
		return "", nil
	case gguf.KVCacheF16:
		return gguf.KVCacheF16, nil
	case gguf.KVCacheQ8_0:
		return gguf.KVCacheQ8_0, nil
	case gguf.KVCacheQ4_0:
		return gguf.KVCacheQ4_0, nil
	default:
		return "", fmt.Errorf("invalid kv_type_%s value %q: valid values are \"\" (unset => f16, the llama.cpp default), %q, %q, or %q. "+
			"tq3_0 is not accepted even on the CUDA fork because the VRAM estimator cannot price it, and a start whose "+
			"footprint cannot be computed must not be admitted",
			side, s, gguf.KVCacheF16, gguf.KVCacheQ8_0, gguf.KVCacheQ4_0)
	}
}

// ValidateSlots validates the `-np` server-slot count. 0 means "unset" and normalizes to 1.
func ValidateSlots(n int) (int, error) {
	if n < 0 || n > MaxSlots {
		return 0, fmt.Errorf("invalid slots value %d: must be between 1 and %d (0 => 1, the default single slot). "+
			"Each slot multiplies the KV cache, so a high count can turn a fitting model into an OOM", n, MaxSlots)
	}
	return n, nil
}

// ValidateSpecDraftMax validates `--spec-draft-n-max`. 0 means "omit the flag" (the engine's own
// default). It is meaningful only for a speculative mode; the caller decides whether to reject it
// as a contradiction when SpecMode is off.
func ValidateSpecDraftMax(n int) (int, error) {
	if n < 0 || n > MaxSpecDraftMax {
		return 0, fmt.Errorf("invalid spec_draft_max value %d: must be between 1 and %d (0 => omit --spec-draft-n-max and use the engine default). "+
			"The validated Qwen3.6-27B MTP profile uses 2", n, MaxSpecDraftMax)
	}
	return n, nil
}

// Normalize clamps the profile to safe/normalized values and returns a copy:
//   - Embedding FORCES SpecMode=off and SpecDraftMax=0 (no decode loop to speculate ahead of).
//   - Slots < 1 => 1.
//   - draft-mtp FORCES Slots=1 (the pinned-build acceptance condition; -np 1).
//   - Batch < 1 => 1.
//
// It does NOT mutate; callers should use the returned copy.
func (p LaunchProfile) Normalize() LaunchProfile {
	if p.Reranking {
		// llama.cpp's --reranking REQUIRES --embedding and --pooling rank together
		// (tools/server/README.md: "Requires ... the --embedding --pooling rank options"), so a
		// caller that set Reranking without also setting Embedding/Pooling still gets a coherent,
		// replayable profile rather than a launch that llama.cpp itself would reject. This runs
		// BEFORE the Embedding speculative-zeroing block below so a rerank profile still gets that
		// zeroing (a reranker is encoder-only, exactly like a plain embedding server).
		p.Embedding = true
		p.Pooling = PoolingRank
	}
	if p.Embedding {
		// An embedding-only server has no autoregressive decode loop, so there is nothing for a
		// draft head or an n-gram self-speculation pool to accelerate. Zero the speculative knobs
		// here so the flag builder cannot emit --spec-type against a server that would reject or
		// ignore it, and so a replayed queued start normalizes identically (M2.5).
		p.SpecMode = SpeculativeOff
		p.SpecDraftMax = 0
	}
	if p.Slots < 1 {
		p.Slots = 1
	}
	if p.SpecMode == SpeculativeDraftMTP {
		p.Slots = 1 // MTP draft path is single-slot on the pinned build.
	}
	if p.Batch < 1 {
		p.Batch = 1
	}
	return p
}

// EffectiveKVTypeK/V resolve each side to f16 when unset (the estimator's default).
func (p LaunchProfile) EffectiveKVTypeK() gguf.KVCacheType {
	if p.KVTypeK != "" {
		return p.KVTypeK
	}
	return gguf.KVCacheF16
}
func (p LaunchProfile) EffectiveKVTypeV() gguf.KVCacheType {
	if p.KVTypeV != "" {
		return p.KVTypeV
	}
	return gguf.KVCacheF16
}

// KVQuantized reports whether either K or V cache is quantized (non-f16). The verdict uses this
// to label a runs_degraded status (KV quant trades a little quality for memory).
func (p LaunchProfile) KVQuantized() bool {
	return p.EffectiveKVTypeK() != gguf.KVCacheF16 || p.EffectiveKVTypeV() != gguf.KVCacheF16
}

// kvQuantUsed is the legacy bool equivalent for callers/flags that still think in terms of
// "is the KV quantized at all": true when either side is non-f16.
func (p LaunchProfile) kvQuantUsed() bool { return p.KVQuantized() }
