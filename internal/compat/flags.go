package compat

import (
	"fmt"
	"strings"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// SpeculativeOff is the empty/off value for the per-start `speculative` knob: today's flags,
// no speculative decoding.
const SpeculativeOff = ""

// SpeculativeNgramCache selects DRAFT-FREE self-speculative decoding via llama.cpp's n-gram
// cache (`--spec-type ngram-cache`). It needs NO second (draft) model and adds ~zero VRAM
// (a small hash pool), so it changes the launch FLAGS only and never the VRAM estimate/admission.
// This is the lowest-risk speculative win on this box (research §1/§5(ii)-1).
//
// Value chosen from the LOCAL b8833 --help dump (research §0, authoritative for this box):
//
//	--spec-type [none|ngram-cache|ngram-simple|ngram-map-k|ngram-map-k4v|ngram-mod]
//	            self-speculative type when NO draft model provided (default none)
//
// v1 exposes only `ngram-cache` (the general-purpose default); the other ngram-* variants and
// the draft-MODEL path (`-md`) are future work (research §5(iii)). NOTE: llama.cpp *master* is
// migrating this surface to `--spec-draft-*`/`draft-eagle3`/`draft-dflash` (research §0 divergence
// flag) — local b8833 does NOT have those, so we emit the b8833 `--spec-type` form. Re-verify on a
// local binary upgrade.
const SpeculativeNgramCache = "ngram-cache"

// ValidateSpeculative validates a per-start `speculative` value. "" (off) and "ngram-cache" are
// accepted; anything else is rejected with a reason listing the valid values (CLAUDE.md: every
// user-facing refusal carries a human-readable reason). Returns the normalized value.
func ValidateSpeculative(s string) (string, error) {
	switch s {
	case SpeculativeOff, SpeculativeNgramCache:
		return s, nil
	default:
		return "", fmt.Errorf("invalid speculative value %q: valid values are %q (off, today's flags) or %q (draft-free self-speculative, --spec-type ngram-cache)",
			s, SpeculativeOff, SpeculativeNgramCache)
	}
}

// ggufRunFlags builds the exact llama-server launch flags for a runnable GGUF verdict, per
// the lmwrapper known-good profile (research/lmwrapper-reuse-notes.md §fork facts + run-*.bat):
//
//	-ngl all -c <CTX> --fit off -fa on [+ -ctk/-ctv <type> when KV-quant needed to fit]
//	[+ --mmproj <path> when a vision companion exists] [+ --spec-type ngram-cache when opted in]
//
// -fa on is gated on the binary reporting flash-attn; KV-quant flags are gated on the binary
// supporting KV quantization (already checked by the caller before requesting kvQuant).
//
// -ngl semantics ([RE-VERIFY 2026-07-07] against the LOCAL binaries' --help, authoritative for
// this box; both the CUDA fork K:\projects\llm_models\llama.cpp-tq3\build\bin\llama-server.exe and
// the winget build report identical text):
//
//	-ngl, --gpu-layers, --n-gpu-layers N   max. number of layers to store in VRAM, either an exact
//	                                       number, 'auto', or 'all' (default: auto)
//
// LocalHub defaults to 'all' (full GPU, never offload to CPU) — the hard default. When the caller
// opts in with allow_cpu_offload=true, we emit 'auto' instead, which lets llama.cpp offload as many
// layers as fit and keep the remainder on CPU. Admission is NOT relaxed by this flag (see
// PlanLlamaLaunch / core StartInstance): a model whose weights exceed VRAM is still refused; 'auto'
// only helps borderline cases the whole-model estimate is pessimistic about.
//
// speculative selects draft-free self-speculative decoding: "" (SpeculativeOff) = today's flags;
// "ngram-cache" (SpeculativeNgramCache) appends `--spec-type ngram-cache`. Both local b8833
// binaries expose --spec-type identically (research §0 flag-surface diff proves no divergence), and
// bestLlamaBinary only ever returns a local b8833-family binary, so this is gated on the request
// value, not a per-binary cap bit. It adds ~zero VRAM, so it does NOT change admission.
func ggufRunFlags(d ModelDescriptor, bin LlamaBinary, ctx int, kvType gguf.KVCacheType, kvQuantUsed bool, allowCPUOffload bool, speculative string) []string {
	ngl := "all"
	if allowCPUOffload {
		ngl = "auto"
	}
	flags := []string{"-ngl", ngl, "-c", fmt.Sprintf("%d", ctx), "--fit", "off"}
	if bin.HasFlashAttn {
		flags = append(flags, "-fa", "on")
	}
	if kvQuantUsed && kvType != gguf.KVCacheF16 {
		// The reference profile uses -ctk q4_0 -ctv tq3_0 on the fork; gguf-parser cannot model
		// tq3_0, so the estimate used q4_0. On the fork we recommend the smaller tq3_0 for the
		// value cache (fork-only), else q4_0 for both.
		flags = append(flags, "-ctk", kvTypeName(kvType))
		if bin.HasTQ3 {
			flags = append(flags, "-ctv", "tq3_0")
		} else {
			flags = append(flags, "-ctv", kvTypeName(kvType))
		}
	}
	if speculative == SpeculativeNgramCache {
		flags = append(flags, "--spec-type", SpeculativeNgramCache)
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
