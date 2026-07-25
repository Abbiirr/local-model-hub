package compat

import (
	"fmt"
	"strings"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// ggufRunFlags builds the exact llama-server launch flags for a runnable GGUF verdict, per
// the lmwrapper known-good profile (research/lmwrapper-reuse-notes.md §fork facts + run-*.bat):
//
//	-ngl all -c <CTX> --fit off -fa on [+ -ctk/-ctv <type> when KV-quant needed to fit]
//	[+ --mmproj <path> when a vision companion exists]
//
// -fa on is gated on the binary reporting flash-attn; KV-quant flags are gated on the binary
// supporting KV quantization (already checked by the caller before requesting kvQuant).
func ggufRunFlags(d ModelDescriptor, bin LlamaBinary, ctx int, kvType gguf.KVCacheType, kvQuantUsed bool) []string {
	flags := []string{"-ngl", "all", "-c", fmt.Sprintf("%d", ctx), "--fit", "off"}
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
