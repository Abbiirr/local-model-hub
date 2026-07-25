package safetensors

// Parameter-count estimation from config.json dimensions. Used ONLY when no authoritative
// weights size (index total_size / shard sum / header) is available — the last rung of the
// spec-digest §4(1) ladder. The count is approximate by construction (it assumes a standard
// decoder-only transformer block) and always downgrades the estimate to Confidence=approximate.
//
// Model (per standard LLaMA-style decoder layer, GQA-aware):
//
//	embeddings   = vocab_size * hidden_size                      (input embedding)
//	lm_head      = vocab_size * hidden_size                      (output; 0 if tied)
//	attn/layer   = hidden*hidden (q_proj)
//	             + hidden*(n_kv_heads*head_dim) (k_proj)
//	             + hidden*(n_kv_heads*head_dim) (v_proj)
//	             + hidden*hidden (o_proj)
//	mlp/layer    = 3 * hidden * intermediate    (gate + up + down, SwiGLU)
//	per_layer    = attn/layer + mlp/layer
//	total        = embeddings + lm_head + n_layers * per_layer
//
// Bias/norm params are negligible (<0.1%) and omitted. Non-SwiGLU MLPs (2 matrices) would be
// slightly over-counted here — conservative for a VRAM estimate. This is a fallback; when the
// real safetensors size is known it is always preferred.
func estimateParamCount(cfg ModelConfig) (int64, bool) {
	h := int64(cfg.HiddenSize)
	nLayers := int64(cfg.NumHiddenLayers)
	inter := int64(cfg.IntermediateSize)
	vocab := int64(cfg.VocabSize)
	headDim := int64(cfg.HeadDimEff())
	kvHeads := int64(cfg.KVHeads())

	if h <= 0 || nLayers <= 0 || headDim <= 0 || kvHeads <= 0 {
		return 0, false
	}

	// Attention projections (GQA: k/v projections are narrower by the GQA ratio).
	kvDim := kvHeads * headDim
	attn := h*h + // q_proj (n_attn_heads*head_dim assumed == hidden for the common case)
		h*kvDim + // k_proj
		h*kvDim + // v_proj
		h*h // o_proj

	// MLP (SwiGLU: gate, up, down). If intermediate_size is unknown, fall back to 4*hidden
	// (the classic GPT ratio) so we still produce a number.
	if inter <= 0 {
		inter = 4 * h
	}
	mlp := 3 * h * inter

	perLayer := attn + mlp
	total := nLayers * perLayer

	// Embeddings + LM head. If vocab is unknown, skip (small relative to layers for big models).
	if vocab > 0 {
		total += vocab * h // input embeddings
		tied := cfg.TieWordEmbeddings != nil && *cfg.TieWordEmbeddings
		if !tied {
			total += vocab * h // untied output head
		}
	}

	if total <= 0 {
		return 0, false
	}
	return total, true
}

// bytesPerParam returns the per-parameter byte cost used in the weights fallback, from the
// quantization method (spec §4(1)): 4-bit AWQ/GPTQ ~0.5, fp8 = 1, else fp16/bf16 = 2. The
// +10% scales overhead for 4-bit is applied by the caller (Estimate), not here.
func bytesPerParam(cfg ModelConfig) float64 {
	switch cfg.Quant() {
	case QuantAWQ, QuantGPTQ:
		return bpp4bit
	case QuantFP8:
		return bppFP8
	default:
		// Unquantized: use the torch_dtype precision. fp32 is rare for LLM weights but honor
		// it if declared; everything else is 2 bytes (fp16/bf16).
		switch normalizeDtype(cfg.TorchDtype) {
		case "float32":
			return 4.0
		case "float8":
			return bppFP8
		default:
			return bppFP16
		}
	}
}
