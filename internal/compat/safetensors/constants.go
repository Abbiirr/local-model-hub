package safetensors

// Estimation constants for the safetensors / vLLM VRAM path.
//
// Every value here is either (a) fixed by physics/spec, or (b) a documented assumption made
// where spec-digest.md §4 (docs/compatibility-engine.md) is SILENT. Case (b) constants are
// flagged "[ASSUMPTION]" and listed in temp-research/wiring-p22.md for orchestrator review.
// All assumptions round toward OVER-estimating required VRAM (conservative), because a false
// "runs" that OOMs is a blocking correctness bug (CLAUDE.md).

const (
	// bytesPerMiB is 1 MiB. VRAM is reported in MiB to match internal/hardware and the
	// verdict vram_*_mb fields (spec-digest §3/§4).
	bytesPerMiB = 1024 * 1024

	// --- Bytes per parameter (weights), spec-digest §4(1) fallback ladder ---
	// Used only when the authoritative weights size (index total_size / shard sum) is absent.
	bppFP16 = 2.0 // fp16 / bf16 — spec §4: "2 (fp16/bf16)"
	bppFP8  = 1.0 // fp8        — spec §4: "1 (fp8)"
	bpp4bit = 0.5 // 4-bit AWQ/GPTQ — spec §4: "~0.5 (4-bit AWQ/GPTQ)"

	// quant4bitScaleOverhead adds ~10% for the scales/zero-points that ride alongside packed
	// 4-bit weights (spec §4: "add ~10% for scales/zeros overhead"). Applied only to the
	// param-count fallback; when index total_size is used it already includes these tensors.
	quant4bitScaleOverhead = 1.10

	// --- KV cache, spec-digest §4(2). All factors below are fixed by the formula, not
	// assumptions. kv_bytes = 2 * n_layers * n_kv_heads * head_dim * seq_len * batch * kvDtypeBytes.
	kvKAndV = 2 // K and V tensors

	// kvDtypeFP16 is the default KV-cache element size (fp16 = 2 bytes). vLLM's default KV
	// dtype follows the model dtype (fp16/bf16 => 2 bytes). fp8 KV cache (1 byte) is opt-in
	// via --kv-cache-dtype and is exposed as a parameter, not defaulted (spec §4: "2 for fp16
	// KV, 1 for fp8 KV cache").
	kvDtypeFP16 = 2

	// --- vLLM runtime overhead, spec-digest §4(3) ---
	// Spec §4: "overhead ≈ 1.5–2.0 GB fixed" (CUDA context + activation buffers + CUDA graph
	// capture). [ASSUMPTION] We pick the LOW end of the stated range for the required-VRAM
	// figure only as the documented constant, but ROUND UP the fixed overhead to the
	// conservative 2.0 GiB by default so the fit test does not under-count. Rationale: the
	// spec gives a range; the correctness bar says prefer conservative. 2.0 GiB is the upper
	// bound of the doc's own range, so it is defensible and never optimistic.
	// Expressed in MiB (GiB * 1024).
	vllmOverheadMiB = 2.0 * 1024 // [ASSUMPTION] 2.0 GiB, upper bound of spec §4's 1.5–2.0 GB range

	// --- Fit margin, spec-digest §4 + PLAN.md resolution 9 ---
	// safetyFactor is the multiplicative estimator fit margin (spec §4:
	// "fits = est_vram <= free_vram_now * safety_factor -- safety_factor ~0.92"; PLAN.md
	// resolution 9 pins it to 0.92). Direction: we SHRINK usable VRAM by multiplying free VRAM
	// by 0.92 (equivalently, a model fits only if it needs <= 92% of free VRAM). We do NOT
	// inflate the requirement; the margin lives on the supply side. This is the estimator's
	// own margin; residency adds its own additive SAFETY_MARGIN_MIB on top later (PLAN res. 9).
	safetyFactor = 0.92

	// --- vLLM gpu-memory-utilization, spec-digest §4(3) / §5 ---
	// vLLMDefaultGPUMemUtil is vLLM's default --gpu-memory-utilization (0.90). vLLM
	// PRE-ALLOCATES this fraction of TOTAL VRAM for weights+activations+KV. We report the
	// minimum utilization that satisfies the requested context so P4's vLLM adapter can pass
	// it as a launch flag. [RE-VERIFY: vLLM default is 0.90 as of the 2026-07-06 revalidation;
	// re-check against the pinned vLLM version before the P4 adapter relies on it.]
	vLLMDefaultGPUMemUtil = 0.90

	// vLLMMaxGPUMemUtil is the practical ceiling we will recommend for gpu-memory-utilization.
	// [ASSUMPTION] Above ~0.97 vLLM commonly OOMs on CUDA-graph capture / fragmentation; we cap
	// the RECOMMENDED value here. If required VRAM needs more than this fraction of total, we
	// report wont_run rather than recommend a knife-edge utilization.
	vLLMMaxGPUMemUtil = 0.97

	// --- MaxContextThatFits quantization ---
	// ctxAlign floors the solved max-context to a multiple of this (task spec: "floor to
	// multiple of 256"). Keeps recommended ctx values clean and slightly conservative.
	ctxAlign = 256
)
