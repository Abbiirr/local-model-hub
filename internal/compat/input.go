package compat

import (
	"strings"

	"github.com/localhub/localhub/internal/compat/gguf"
	"github.com/localhub/localhub/internal/compat/safetensors"
)

// Format is the artifact format the rule engine routes on. Mirrors inventory.Format
// (gguf | safetensors | pytorch | ollama); duplicated here so this package does not import
// internal/inventory (avoids a cycle: inventory does not depend on compat, and neither
// should the reverse for the descriptor).
type Format string

const (
	FormatGGUF        Format = "gguf"
	FormatSafetensors Format = "safetensors"
	FormatPyTorch     Format = "pytorch"
	FormatOllama      Format = "ollama"
)

// ModelDescriptor is the normalized model the rule engine evaluates. The caller (core.Service
// / CompatPreview) builds it from either an inventory `models` row or a remote reference, and
// resolves the estimator source (a GGUF Source or safetensors config/weights) alongside.
type ModelDescriptor struct {
	// ID/DisplayName identify the model in the verdict (Verdict.Model).
	ID          string
	DisplayName string

	// Source is the provenance (ollama | huggingface | local_path | direct_url). Used to
	// decide Ollama routing (only ollama-source GGUF models route to Ollama).
	Source string

	// Format routes the whole verdict: GGUF -> llama.cpp/Ollama; safetensors -> vLLM.
	Format Format

	// Quantization is the best-effort quant label (q4_k_m, tq3_4s, bf16, awq, ...). Used to
	// detect TQ3_* (needs the CUDA fork) and, for safetensors, quant method.
	Quantization string

	// LocalPath is the on-disk artifact (a .gguf file, or the model root dir for safetensors).
	// Empty for a remote-only descriptor.
	LocalPath string

	// Repo/Revision/File identify a remote HF model (repo + optional file for GGUF, + revision).
	Repo     string
	Revision string
	File     string // remote GGUF filename within the repo

	// HasMMProj is true when a vision projector companion (mmproj) exists beside a GGUF; it
	// adds --mmproj to the llama.cpp run flags.
	HasMMProj  bool
	MMProjPath string

	// --- Estimator inputs, resolved by the caller ---

	// GGUFSource is the resolved gguf.Source (LocalFile / HFRepo / URL) for a GGUF model. Zero
	// value (kind 0 + empty path) means "not a GGUF / not resolved".
	GGUFSource gguf.Source

	// STConfig / STWeights are the resolved safetensors config + weights for a safetensors
	// model. STConfig.NumHiddenLayers==0 means "not resolved".
	STConfig  safetensors.ModelConfig
	STWeights safetensors.WeightsInfo
	// STConfigResolved marks whether the safetensors config was actually fetched/parsed (so a
	// genuinely empty config is distinguishable from "not attempted").
	STConfigResolved bool
}

// IsTQ3 reports whether the model is a TQ3_* quant (needs a has_tq3 llama.cpp binary — the
// CUDA fork). Matches the quant label OR the filename, case-insensitively.
func (d ModelDescriptor) IsTQ3() bool {
	q := strings.ToLower(d.Quantization)
	if strings.HasPrefix(q, "tq3") {
		return true
	}
	// Fall back to the filename/local path (e.g. "...-TQ3_4S.gguf").
	name := strings.ToLower(d.File)
	if name == "" {
		name = strings.ToLower(d.LocalPath)
	}
	return strings.Contains(name, "tq3")
}

// IsRemote reports whether the model has no local artifact (evaluated from remote metadata
// only). A remote GGUF still parses via range reads; a remote safetensors uses a fetched
// config.
func (d ModelDescriptor) IsRemote() bool {
	return d.LocalPath == "" && d.Repo != ""
}

// EngineAvailability captures what the engine-detection layer found: which llama.cpp binaries
// exist with which capabilities, whether Ollama is reachable, and the vLLM mode/reachability.
// The caller builds this from core.Service.Engines() (read-only detection).
type EngineAvailability struct {
	// LlamaBinaries is one entry per detected llama-server binary.
	LlamaBinaries []LlamaBinary
	// OllamaReachable is true when the Ollama daemon answered detection.
	OllamaReachable bool
	// VLLMMode is the configured vLLM launch mode ("docker" | "attach" | ""). Docker mode
	// implies a cold-start warning (runs_degraded) per the task spec.
	VLLMMode string
	// VLLMAvailable is true when vLLM is usable (attach endpoint reachable, or a docker image
	// present). false => vLLM path can only be a hypothetical verdict.
	VLLMAvailable bool
}

// LlamaBinary is one detected llama-server binary with its capabilities. Built from
// engines.EngineStatus (llama.cpp) — Detail is a short human label, HasTQ3/HasFlashAttn come
// from the detected capabilities, HasCUDA from the detected backends.
type LlamaBinary struct {
	Path         string
	Detail       string // e.g. "CUDA fork" | "winget (Vulkan/CPU)"
	Detected     bool
	HasTQ3       bool
	HasFlashAttn bool
	HasCUDA      bool
	// SupportsKVQuant is true when the binary can quantize the KV cache (-ctk/-ctv). All
	// modern builds do; the fork additionally supports tq3_0. We gate KV-quant run flags on it.
	SupportsKVQuant bool
}

// bestLlamaBinary picks the llama-server binary to run this descriptor on:
//   - A TQ3_* model REQUIRES a detected has_tq3 binary; returns (binary, true) or (zero, false).
//   - A non-TQ3 model prefers a detected CUDA binary (fastest on this box), else any detected
//     binary. Returns (binary, true) or (zero, false) if none detected.
func (a EngineAvailability) bestLlamaBinary(requireTQ3 bool) (LlamaBinary, bool) {
	if requireTQ3 {
		for _, b := range a.LlamaBinaries {
			if b.Detected && b.HasTQ3 {
				return b, true
			}
		}
		return LlamaBinary{}, false
	}
	// Prefer a CUDA binary.
	var anyDetected *LlamaBinary
	for i := range a.LlamaBinaries {
		b := a.LlamaBinaries[i]
		if !b.Detected {
			continue
		}
		if b.HasCUDA {
			return b, true
		}
		if anyDetected == nil {
			anyDetected = &a.LlamaBinaries[i]
		}
	}
	if anyDetected != nil {
		return *anyDetected, true
	}
	return LlamaBinary{}, false
}

// tq3ForkName returns a human name for the missing TQ3 fork, for the wont_run reason when a
// TQ3 model has no has_tq3 binary available.
func (a EngineAvailability) tq3ForkName() string {
	// If a non-TQ3 binary was detected, name it so the reason explains what IS available.
	for _, b := range a.LlamaBinaries {
		if b.Detected && !b.HasTQ3 {
			return b.Detail
		}
	}
	return "the winget/Vulkan build"
}
