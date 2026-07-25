package safetensors

import (
	"fmt"
	"math"
)

// Confidence labels how trustworthy an estimate is.
//   - ConfidenceExact:       weights size came from an authoritative source (index/shards/header).
//   - ConfidenceApproximate: weights size was derived from a param-count fallback, OR a
//     required dimension was missing and defaulted.
type Confidence string

const (
	ConfidenceExact       Confidence = "exact"
	ConfidenceApproximate Confidence = "approximate"
)

// Params bundles the per-request estimation inputs. Ctx and Batch describe the serving
// workload; KVDtypeBytes selects fp16 (2, default) vs fp8 (1) KV cache.
type Params struct {
	Ctx          int // context length (tokens) to serve; must be > 0
	Batch        int // concurrent sequences; default 1 if <= 0
	KVDtypeBytes int // 2 = fp16 KV (default), 1 = fp8 KV cache; other values => 2
}

// Estimate is the structured VRAM estimate for a safetensors/vLLM model at a given ctx.
// All *MiB fields are mebibytes. It is the safetensors-path analogue of the GGUF estimator's
// output and feeds the compatibility verdict (spec-digest §4).
type Estimate struct {
	WeightsMiB    float64 // model weights
	KVCacheMiB    float64 // KV cache at (Ctx, Batch, KVDtype)
	OverheadMiB   float64 // fixed vLLM runtime overhead (CUDA ctx + activations + graph capture)
	TotalMiB      float64 // WeightsMiB + KVCacheMiB + OverheadMiB (required_vram)
	BytesPerParam float64 // effective bytes/param used for weights (fallback path) or 0 if authoritative size used

	Ctx   int // the context this estimate was computed at
	Batch int // the batch this estimate was computed at

	IsGQA    bool    // grouped-query attention (fewer KV heads than attn heads)
	GQARatio float64 // n_attn_heads / n_kv_heads (>=1); KV over-estimation factor of the naive formula

	Confidence Confidence // exact | approximate
	Notes      []string   // human-readable assumptions/derivations (spec: "carry a reason")
}

// GPUMemUtilGuidance is the vLLM-specific launch guidance (spec-digest §4(3)/§5). vLLM
// pre-allocates gpu_memory_utilization * TOTAL VRAM; P4's vLLM adapter consumes MinUtil as
// the --gpu-memory-utilization launch flag. Fits==false means required VRAM exceeds the
// recommended ceiling of total VRAM even at max utilization.
type GPUMemUtilGuidance struct {
	RequiredMiB   float64 // TotalMiB from the Estimate (weights+kv+overhead)
	TotalVRAMMiB  float64 // total device VRAM the fraction is taken of
	MinUtil       float64 // minimum --gpu-memory-utilization that fits RequiredMiB, clamped to [.., MaxUtil]
	RecommendUtil float64 // MinUtil rounded up to a clean step, capped at vLLMMaxGPUMemUtil
	Fits          bool    // whether RequiredMiB fits within RecommendUtil * TotalVRAMMiB
	Notes         []string
}

// EstimateVRAM computes the VRAM estimate for cfg + weights at the given params. It never
// panics on a sparse config: missing dimensions are defaulted and downgrade Confidence to
// approximate, with a Note explaining each substitution. Returns an error only when the
// config lacks enough structure to compute a KV cache at all (no layers, no heads/head_dim).
func EstimateVRAM(cfg ModelConfig, w WeightsInfo, p Params) (Estimate, error) {
	if p.Ctx <= 0 {
		return Estimate{}, fmt.Errorf("safetensors: ctx must be > 0, got %d", p.Ctx)
	}
	batch := p.Batch
	if batch <= 0 {
		batch = 1
	}
	kvBytes := p.KVDtypeBytes
	if kvBytes != 1 && kvBytes != 2 {
		kvBytes = kvDtypeFP16
	}

	var notes []string
	conf := ConfidenceExact

	headDim := cfg.HeadDimEff()
	if headDim <= 0 {
		return Estimate{}, fmt.Errorf("safetensors: cannot determine head_dim (need head_dim or hidden_size+num_attention_heads)")
	}
	if cfg.HeadDim != nil && *cfg.HeadDim > 0 &&
		cfg.NumAttentionHeads > 0 && cfg.HiddenSize > 0 &&
		*cfg.HeadDim != cfg.HiddenSize/cfg.NumAttentionHeads {
		notes = append(notes, fmt.Sprintf("using explicit head_dim=%d (differs from hidden_size/num_attention_heads=%d)",
			*cfg.HeadDim, cfg.HiddenSize/cfg.NumAttentionHeads))
	} else if cfg.HeadDim == nil {
		notes = append(notes, fmt.Sprintf("head_dim derived = hidden_size/num_attention_heads = %d", headDim))
	}

	nLayers := cfg.NumHiddenLayers
	if nLayers <= 0 {
		return Estimate{}, fmt.Errorf("safetensors: num_hidden_layers missing or zero")
	}
	kvHeads := cfg.KVHeads()
	if kvHeads <= 0 {
		return Estimate{}, fmt.Errorf("safetensors: cannot determine kv heads (need num_key_value_heads or num_attention_heads)")
	}
	if cfg.NumKeyValueHeads == nil {
		notes = append(notes, fmt.Sprintf("num_key_value_heads absent => MHA, n_kv_heads = num_attention_heads = %d", kvHeads))
	}

	// --- (2) KV cache, spec-digest §4(2): GQA-aware, scales with n_kv_heads NOT n_attn_heads.
	// kv_bytes = 2 * n_layers * n_kv_heads * head_dim * seq_len * batch * kv_dtype_bytes
	kvBytesTotal := float64(kvKAndV) *
		float64(nLayers) *
		float64(kvHeads) *
		float64(headDim) *
		float64(p.Ctx) *
		float64(batch) *
		float64(kvBytes)
	kvMiB := kvBytesTotal / bytesPerMiB

	// --- (1) Weights, spec-digest §4(1): authoritative size preferred; else param-count fallback.
	var weightsMiB, bpp float64
	if w.TotalBytes > 0 {
		weightsMiB = float64(w.TotalBytes) / bytesPerMiB
		notes = append(notes, fmt.Sprintf("weights from %s = %.0f MiB (authoritative)", w.Source, weightsMiB))
	} else {
		// Fallback: estimate parameter count from architecture, then * bytes-per-param.
		params, ok := estimateParamCount(cfg)
		if !ok {
			return Estimate{}, fmt.Errorf("safetensors: no authoritative weights size and cannot estimate param count (missing dims)")
		}
		bpp = bytesPerParam(cfg)
		weightsBytes := float64(params) * bpp
		if cfg.Quant() == QuantAWQ || cfg.Quant() == QuantGPTQ {
			weightsBytes *= quant4bitScaleOverhead // +10% scales/zeros (spec §4)
		}
		weightsMiB = weightsBytes / bytesPerMiB
		conf = ConfidenceApproximate
		notes = append(notes, fmt.Sprintf(
			"weights estimated from ~%.2fB params * %.2f bytes/param%s = %.0f MiB (approximate — no index/shard size)",
			float64(params)/1e9, bpp,
			map[bool]string{true: " * 1.10 quant scales", false: ""}[cfg.Quant() == QuantAWQ || cfg.Quant() == QuantGPTQ],
			weightsMiB))
	}

	// --- (3) Overhead, spec-digest §4(3): fixed vLLM runtime overhead.
	overheadMiB := float64(vllmOverheadMiB)
	notes = append(notes, fmt.Sprintf("vLLM fixed overhead = %.0f MiB (CUDA ctx + activations + graph capture; spec §4 range 1.5-2.0 GiB, upper bound used)", overheadMiB))

	total := weightsMiB + kvMiB + overheadMiB

	return Estimate{
		WeightsMiB:    round1(weightsMiB),
		KVCacheMiB:    round1(kvMiB),
		OverheadMiB:   round1(overheadMiB),
		TotalMiB:      round1(total),
		BytesPerParam: bpp,
		Ctx:           p.Ctx,
		Batch:         batch,
		IsGQA:         cfg.IsGQA(),
		GQARatio:      round2(cfg.GQARatio()),
		Confidence:    conf,
		Notes:         notes,
	}, nil
}

// kvMiBPerToken returns the KV-cache MiB consumed by ONE context token at batch=1 with the
// given kv dtype. KV is exactly linear in ctx (and in batch), so this is the slope used by
// MaxContextThatFits and by the linearity property test. head_dim/layers/kv_heads come from cfg.
func kvMiBPerToken(cfg ModelConfig, kvDtypeBytes int) float64 {
	if kvDtypeBytes != 1 && kvDtypeBytes != 2 {
		kvDtypeBytes = kvDtypeFP16
	}
	headDim := cfg.HeadDimEff()
	perToken := float64(kvKAndV) *
		float64(cfg.NumHiddenLayers) *
		float64(cfg.KVHeads()) *
		float64(headDim) *
		float64(kvDtypeBytes)
	return perToken / bytesPerMiB
}

// FitsInVRAM reports whether the estimate fits in the given free VRAM (MiB) under the
// estimator's multiplicative safety_factor (0.92, PLAN.md res. 9). The margin shrinks the
// USABLE VRAM (free * 0.92); the requirement is not inflated. See constants.go.
func (e Estimate) FitsInVRAM(freeVRAMMiB float64) bool {
	return e.TotalMiB <= freeVRAMMiB*safetyFactor
}

// MaxContextThatFits solves the KV budget for the largest context that fits in freeVRAMMiB,
// holding weights+overhead fixed. It is the closed-form inverse of the (linear-in-ctx) KV
// formula:
//
//	usable        = freeVRAMMiB * safety_factor
//	kv_budget     = usable - weights - overhead
//	max_ctx_raw   = kv_budget / (kv_MiB_per_token * batch)
//	result        = floor(max_ctx_raw to multiple of 256), capped at max_position_embeddings
//
// Returns 0 if weights+overhead alone already exceed usable VRAM (model can't load at any
// ctx). batch defaults to 1 when <= 0. If max_position_embeddings is 0/unknown, no cap is
// applied beyond the KV budget.
func MaxContextThatFits(cfg ModelConfig, w WeightsInfo, freeVRAMMiB float64, batch, kvDtypeBytes int) int {
	if batch <= 0 {
		batch = 1
	}
	// weights + overhead are ctx-independent; compute them via a ctx=1 estimate's components.
	base, err := EstimateVRAM(cfg, w, Params{Ctx: 1, Batch: batch, KVDtypeBytes: kvDtypeBytes})
	if err != nil {
		return 0
	}
	fixed := base.WeightsMiB + base.OverheadMiB
	usable := freeVRAMMiB * safetyFactor
	kvBudget := usable - fixed
	if kvBudget <= 0 {
		return 0
	}
	perToken := kvMiBPerToken(cfg, kvDtypeBytes) * float64(batch)
	if perToken <= 0 {
		return 0
	}
	maxCtxRaw := kvBudget / perToken
	maxCtx := int(math.Floor(maxCtxRaw))
	// floor to multiple of ctxAlign (conservative).
	maxCtx = (maxCtx / ctxAlign) * ctxAlign
	if maxCtx < 0 {
		maxCtx = 0
	}
	// cap at max_position_embeddings when known.
	if cfg.MaxPositionEmbeds > 0 && maxCtx > cfg.MaxPositionEmbeds {
		maxCtx = cfg.MaxPositionEmbeds
	}
	return maxCtx
}

// GPUMemUtil computes the vLLM --gpu-memory-utilization guidance for this estimate against a
// device with totalVRAMMiB total memory. vLLM pre-allocates util * TOTAL; so the minimum
// util that holds RequiredMiB is RequiredMiB/total, clamped up to a clean step and capped at
// vLLMMaxGPUMemUtil. Fits is false when RequiredMiB needs more than the cap allows.
func (e Estimate) GPUMemUtil(totalVRAMMiB float64) GPUMemUtilGuidance {
	g := GPUMemUtilGuidance{
		RequiredMiB:  e.TotalMiB,
		TotalVRAMMiB: totalVRAMMiB,
	}
	if totalVRAMMiB <= 0 {
		g.Notes = append(g.Notes, "total VRAM unknown; cannot compute gpu-memory-utilization")
		return g
	}
	minUtil := e.TotalMiB / totalVRAMMiB
	g.MinUtil = round3(minUtil)

	// Recommend: round minUtil UP to a 0.01 step (never below vLLM's own default 0.90 when it
	// fits comfortably, so the KV cache gets breathing room), capped at the safe ceiling.
	rec := math.Ceil(minUtil*100) / 100
	if rec < vLLMDefaultGPUMemUtil && minUtil <= vLLMDefaultGPUMemUtil {
		// If the model fits under the default, recommend the default so vLLM sizes a healthy
		// KV cache rather than the bare minimum. (spec §4: vLLM FILLS the fraction with KV.)
		rec = vLLMDefaultGPUMemUtil
	}
	if rec > vLLMMaxGPUMemUtil {
		rec = vLLMMaxGPUMemUtil
	}
	g.RecommendUtil = round3(rec)
	g.Fits = e.TotalMiB <= rec*totalVRAMMiB
	if !g.Fits {
		g.Notes = append(g.Notes, fmt.Sprintf(
			"required %.0f MiB exceeds %.0f%% of %.0f MiB total; needs smaller quant / tensor-parallel / CPU offload",
			e.TotalMiB, vLLMMaxGPUMemUtil*100, totalVRAMMiB))
	} else {
		g.Notes = append(g.Notes, fmt.Sprintf(
			"set --gpu-memory-utilization %.2f (min fit %.3f); vLLM pre-allocates that fraction of the %0.f MiB device",
			g.RecommendUtil, g.MinUtil, totalVRAMMiB))
	}
	return g
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }
func round2(x float64) float64 { return math.Round(x*100) / 100 }
func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
