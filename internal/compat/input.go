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

	// HasMTP is true when the GGUF embeds a multi-token-prediction (next-n / draft) head — the
	// tensors `--spec-type draft-mtp` loads (verified convention on the Qwen3.6 target:
	// `blk.64.nextn.*`, M3.2). Set by callers that have inspected the file (manifest/launch);
	// when false, PlanLlamaLaunch re-detects it via gguf.InspectMTP before gating MTP.
	HasMTP bool

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
//
// NOTE (M3.7): HasCUDA is a REPORTING field only (used for the /api/engines label), NOT a
// selection criterion. Selection is capability-requirements + configured order (see
// bestLlamaBinary / BinaryRequirements).
type LlamaBinary struct {
	Path         string
	Detail       string // e.g. "CUDA fork" | "winget (Vulkan/CPU)"
	Detected     bool
	HasTQ3       bool
	HasFlashAttn bool
	HasCUDA      bool
	// HasSpecMTP is true when the binary's --help advertises `draft-mtp` in --spec-type (the
	// embedded multi-token-prediction head). Verified on b10106 (2026-07-25). MTP launch requires
	// BOTH this AND the model embedding MTP tensors (ModelDescriptor.HasMTP, M3.2/M3.3).
	HasSpecMTP bool
	// HasSpecDraftMax is true when the binary exposes `--spec-draft-n-max` (the MTP draft-token
	// count). Verified on b10106.
	HasSpecDraftMax bool
	// SupportsKVQuant is true when the binary can quantize the KV cache (-ctk/-ctv). All
	// modern builds do; the fork additionally supports tq3_0. We gate KV-quant run flags on it.
	SupportsKVQuant bool
}

// BinaryRequirements captures the capabilities a llama-server binary MUST have to run a given
// model/launch profile. Selection picks the first DETECTED binary (in CONFIGURED order) that
// satisfies every set requirement; an empty requirement set matches any detected binary.
//
// This replaces the old "prefer HasCUDA" heuristic (M3.7/A): HasCUDA is now an accurate
// reporting field only (M3.7/B fixes its detection for modern dynamic-backend builds), never a
// selection criterion. Configured order is the tie-break — which is what operators expect and
// what lets b10106 (configured first) win for a non-TQ3 model even when a has_cuda fork is also
// present. Requirements are extensible: today only TQ3 (a TQ3_* model needs a has_tq3 binary);
// M3.1+ will add has_spec_mtp and independent-K/V requirements for the MTP profile here.
type BinaryRequirements struct {
	// TQ3 is set when the model is a TQ3_* quant that needs the has_tq3 CUDA fork.
	TQ3 bool
	// SpecMTP is set when the launch profile requests draft-mtp and so requires a has_spec_mtp
	// binary (M3.3). The model side (embedded MTP tensors) is checked separately by
	// PlanLlamaLaunch; selection only guarantees the binary side here.
	SpecMTP bool
}

// supports reports whether binary b satisfies every requirement in r.
func (b LlamaBinary) supports(r BinaryRequirements) bool {
	if r.TQ3 && !b.HasTQ3 {
		return false
	}
	if r.SpecMTP && !b.HasSpecMTP {
		return false
	}
	return true
}

// requirementsForModel derives the binary requirements a model imposes. Today: a TQ3_* quant
// REQUIRES a has_tq3 binary (the CUDA fork); any other model imposes no hard requirement and
// is matched by the first detected binary in configured order. MTP (has_spec_mtp) is a
// PROFILE requirement, not a model-inherent one, so it is added by requirementsForProfile.
func requirementsForModel(d ModelDescriptor) BinaryRequirements {
	return BinaryRequirements{TQ3: d.IsTQ3()}
}

// requirementsForProfile folds profile-derived requirements (MTP) into the model-derived ones.
// A draft-mtp profile requires a has_spec_mtp binary (M3.3). This is the single derivation point
// shared by the verdict path and the launch path.
func requirementsForProfile(d ModelDescriptor, p LaunchProfile) BinaryRequirements {
	r := requirementsForModel(d)
	if p.Normalize().SpecMode == SpeculativeDraftMTP {
		r.SpecMTP = true
	}
	return r
}

// bestLlamaBinary selects the llama-server binary to run a model that imposes reqs:
//   - Scan LlamaBinaries in CONFIGURED order; return the first DETECTED binary that satisfies
//     every requirement in reqs.
//   - If none satisfies, return (zero, false).
//
// Configured order is respected (M3.6): a non-TQ3 model with b10106 configured first selects
// b10106 even when a has_cuda fork is also detected — the operator's listed order is the
// preference, and it removes the class of bug where a newer binary loses to an older one for
// cosmetic banner reasons. HasCUDA is deliberately NOT consulted (M3.7/A): the old heuristic
// made a modern CUDA build that advertises no backends lose to an older fork. EngineAvailability.
// LlamaBinaries preserves configured order (built in detection order from the llamacpp adapter,
// which iterates cfg.Engines.LlamaCPP.BinaryPaths as merged/ordered by config.Load).
func (a EngineAvailability) bestLlamaBinary(reqs BinaryRequirements) (LlamaBinary, bool) {
	for i := range a.LlamaBinaries {
		b := a.LlamaBinaries[i]
		if b.Detected && b.supports(reqs) {
			return b, true
		}
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

// listBinaryDetails returns a compact list of the detected binaries' human labels, for refusal
// reasons when no binary satisfies a capability requirement (e.g. draft-mtp needs has_spec_mtp).
// Empty when nothing is detected.
func (a EngineAvailability) listBinaryDetails() string {
	var ds []string
	for _, b := range a.LlamaBinaries {
		if !b.Detected {
			continue
		}
		label := b.Detail
		if label == "" {
			label = b.Path
		}
		ds = append(ds, label)
	}
	if len(ds) == 0 {
		return "(none detected)"
	}
	return strings.Join(ds, ", ")
}
