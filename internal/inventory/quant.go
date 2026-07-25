package inventory

import (
	"regexp"
	"strings"
)

// quantPattern matches common GGUF/quantization labels embedded in filenames. It is
// deliberately tolerant: order matters (longer/more-specific alternatives first) and the
// match is case-insensitive. An empty result means "unknown" — the scanner records that as
// NULL rather than guessing, per the correctness bar (never invent metadata).
//
// Covered families (verified against real filenames on the reference box + llama.cpp
// ggml_type names, github.com/ggml-org/llama.cpp — re-verify if new quant types appear):
//   - K-quants:   Q2_K, Q3_K_S/M/L, Q4_K_S/M, Q5_K_S/M, Q6_K, Q8_K
//   - legacy:     Q4_0, Q4_1, Q5_0, Q5_1, Q8_0
//   - i-quants:   IQ1_S/M, IQ2_XXS/XS/S/M, IQ3_XXS/XS/S/M, IQ4_XS/NL
//   - TQ (fork):  TQ1_0, TQ2_0, TQ3_0, and the model-label forms TQ3_1S / TQ3_4S used by
//     the local CUDA fork (research/lmwrapper-reuse-notes.md). gguf-parser
//     cannot parse TQ3_* but the FILENAME label is still a valid quant tag.
//   - float:     F16/FP16, BF16, F32/FP32, F8/FP8
//   - GPTQ/AWQ:  awq, gptq (safetensors quantized), int8/int4
//
// The regex captures the label; callers normalize case (lowercase) to match the DDL
// examples (q4_k_m, bf16, ...).
var quantPattern = regexp.MustCompile(`(?i)\b(` +
	// TQ model-label forms first (TQ3_4S etc.) then base TQ types.
	`TQ[123]_[0-9]+S|TQ[123]_[0-9]|` +
	// i-quants (specific suffixes before generic).
	`IQ[1234]_(?:XXS|XS|NL|S|M)|IQ[1234]|` +
	// K-quants with size suffix, then bare _K.
	`Q[2-8]_K_[SML]|Q[2-8]_K|` +
	// legacy fixed quants.
	`Q[458]_[01]|Q[458]|` +
	// floats.
	`BF16|FP16|F16|FP32|F32|FP8|F8|` +
	// safetensors quantizers.
	`AWQ|GPTQ|INT8|INT4` +
	`)\b`)

// DetectQuant returns a normalized quantization label parsed from a filename (or any
// string containing one), or "" if none is recognized. The returned label is lowercased so
// it matches the `models.quantization` examples in the DDL (q4_k_m, bf16, tq3_4s).
//
// Example: "Qwen3.6-27B-TQ3_4S.gguf" -> "tq3_4s"; "model.Q4_K_M.gguf" -> "q4_k_m";
// "mmproj-BF16.gguf" -> "bf16"; "unknown-model.gguf" -> "".
func DetectQuant(name string) string {
	m := quantPattern.FindString(name)
	if m == "" {
		return ""
	}
	return strings.ToLower(m)
}
