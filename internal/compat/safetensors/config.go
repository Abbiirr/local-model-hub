// Package safetensors estimates VRAM for safetensors / vLLM models from small JSON
// metadata (config.json + the safetensors index) WITHOUT downloading the weights.
//
// This is the non-GGUF path of the compatibility engine (spec-digest.md §4). GGUF is
// handled by gguf-parser-go in a sibling package; here we must do the math ourselves
// because gguf-parser is GGUF-only. The estimate is deliberately conservative — a false
// "runs" that OOMs is a blocking correctness bug (CLAUDE.md), so where the spec is silent
// we round in the direction that over-estimates required VRAM.
//
// Units: VRAM is reported in MiB throughout, matching internal/hardware (nvidia-smi
// --nounits reports MiB) and the verdict's vram_*_mb fields (spec-digest §3/§4). 1 MiB =
// 1024*1024 bytes.
package safetensors

import (
	"encoding/json"
	"fmt"
	"strings"
)

// QuantMethod is the model-level weight quantization scheme, read from
// config.json.quantization_config.quant_method. It drives bytes-per-param when the
// authoritative weights size is unavailable.
type QuantMethod string

const (
	QuantNone QuantMethod = ""     // unquantized: precision comes from torch_dtype
	QuantAWQ  QuantMethod = "awq"  // 4-bit weight quant
	QuantGPTQ QuantMethod = "gptq" // 4-bit (or 8-bit) weight quant
	QuantFP8  QuantMethod = "fp8"  // 8-bit float weights
)

// quantizationConfig is the defensive subset of config.json.quantization_config we read.
// Different quantizers spell fields differently (bits vs w_bit); we read the common ones.
type quantizationConfig struct {
	QuantMethod string `json:"quant_method"` // "awq" | "gptq" | "fp8" | "compressed-tensors" ...
	Bits        *int   `json:"bits"`         // GPTQ / compressed-tensors
	WBit        *int   `json:"w_bit"`        // AWQ spelling
}

// ModelConfig is the defensive parse of a HuggingFace config.json. Every field is
// optional; a partial or unusual config must degrade to a documented default, never panic.
// Field semantics follow the HF transformers config convention.
type ModelConfig struct {
	HiddenSize         int    `json:"hidden_size"`
	NumHiddenLayers    int    `json:"num_hidden_layers"`
	NumAttentionHeads  int    `json:"num_attention_heads"`
	NumKeyValueHeads   *int   `json:"num_key_value_heads"` // pointer: absent => MHA (== NumAttentionHeads)
	HeadDim            *int   `json:"head_dim"`            // pointer: absent => hidden_size/num_attention_heads
	IntermediateSize   int    `json:"intermediate_size"`
	VocabSize          int    `json:"vocab_size"`
	TorchDtype         string `json:"torch_dtype"`
	MaxPositionEmbeds  int    `json:"max_position_embeddings"`
	Architectures      []string
	ModelType          string              `json:"model_type"`
	TieWordEmbeddings  *bool               `json:"tie_word_embeddings"`
	QuantizationConfig *quantizationConfig `json:"quantization_config"`

	// raw architectures capture (some configs omit; keep tolerant)
	ArchRaw []string `json:"architectures"`
}

// ParseConfig parses a config.json byte slice defensively. It returns an error only when
// the bytes are not valid JSON at all; a syntactically valid but semantically sparse config
// (missing dimensions) parses fine and is validated later by Estimate.
func ParseConfig(raw []byte) (ModelConfig, error) {
	var c ModelConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return ModelConfig{}, fmt.Errorf("safetensors: parse config.json: %w", err)
	}
	c.Architectures = c.ArchRaw
	return c, nil
}

// KVHeads returns the number of key/value heads, applying the GQA/MHA default: when
// num_key_value_heads is absent (nil) the model is multi-head attention and n_kv == n_attn.
// A present-but-zero value is treated as absent (defensive).
func (c ModelConfig) KVHeads() int {
	if c.NumKeyValueHeads != nil && *c.NumKeyValueHeads > 0 {
		return *c.NumKeyValueHeads
	}
	return c.NumAttentionHeads
}

// HeadDimEff returns the effective per-head dimension. It prefers an explicit head_dim
// field (some architectures — e.g. certain Gemma/Cohere variants — use a head_dim that is
// NOT hidden_size/num_attention_heads), and otherwise derives hidden_size/num_attention_heads.
// Returns 0 if it cannot be determined (no head_dim and num_attention_heads == 0).
func (c ModelConfig) HeadDimEff() int {
	if c.HeadDim != nil && *c.HeadDim > 0 {
		return *c.HeadDim
	}
	if c.NumAttentionHeads > 0 && c.HiddenSize > 0 {
		return c.HiddenSize / c.NumAttentionHeads
	}
	return 0
}

// IsGQA reports whether the model uses grouped-query attention (fewer KV heads than
// attention heads). MHA (kv == attn) and MQA (kv == 1) are both non-strict cases; we call
// anything with kv < attn "GQA" for the verdict flag.
func (c ModelConfig) IsGQA() bool {
	return c.KVHeads() < c.NumAttentionHeads
}

// GQARatio is num_attention_heads / num_kv_heads (>= 1). It is the factor by which the naive
// (hidden_size-based) KV formula OVER-estimates. 1.0 for MHA. Returns 1.0 defensively when
// KV heads is 0.
func (c ModelConfig) GQARatio() float64 {
	kv := c.KVHeads()
	if kv <= 0 {
		return 1.0
	}
	return float64(c.NumAttentionHeads) / float64(kv)
}

// Quant returns the normalized weight-quantization method from quantization_config, or
// QuantNone if the model is unquantized. "compressed-tensors" with 4/8 bits is mapped to
// the closest of AWQ/GPTQ/FP8 for bytes-per-param purposes (documented approximation).
func (c ModelConfig) Quant() QuantMethod {
	if c.QuantizationConfig == nil {
		return QuantNone
	}
	m := strings.ToLower(strings.TrimSpace(c.QuantizationConfig.QuantMethod))
	switch m {
	case "awq":
		return QuantAWQ
	case "gptq":
		return QuantGPTQ
	case "fp8":
		return QuantFP8
	case "compressed-tensors", "compressed_tensors":
		// Approximate by bit-width when the generic compressed-tensors quantizer is used.
		if b := c.quantBits(); b == 8 {
			return QuantFP8
		}
		return QuantGPTQ // 4-bit-ish integer path
	default:
		// Unknown quantizer: treat as unquantized precision so we don't UNDER-estimate
		// (unquantized bytes-per-param is the larger, more conservative assumption).
		return QuantNone
	}
}

// quantBits returns the configured bit-width (bits or w_bit), or 0 if unspecified.
func (c ModelConfig) quantBits() int {
	if c.QuantizationConfig == nil {
		return 0
	}
	if c.QuantizationConfig.Bits != nil {
		return *c.QuantizationConfig.Bits
	}
	if c.QuantizationConfig.WBit != nil {
		return *c.QuantizationConfig.WBit
	}
	return 0
}
