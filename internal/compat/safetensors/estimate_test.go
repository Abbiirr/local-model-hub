package safetensors

import (
	"os"
	"path/filepath"
	"testing"
)

// referenceGPUMiB is the reference box's total VRAM (RTX 4060 Ti 16 GB) as reported by the
// hardware probe in MiB (temp-research/wiring-p12.md: 16380 MiB total). Fit tests use free
// VRAM at/below this.
const referenceGPUTotalMiB = 16380

// referenceGPUFreeMiB is a representative free figure on a fresh box (probe reported 13287
// free with the desktop idle). The boundary test below uses the total for a clean anchor.
const referenceGPUFreeMiB = 16380

func loadConfig(t *testing.T, name string) ModelConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return cfg
}

func loadIndex(t *testing.T, name string) WeightsInfo {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	w, ok := ParseIndexTotalSize(raw)
	if !ok {
		t.Fatalf("index %s had no total_size", name)
	}
	return w
}

// --- (1) Llama-3.1-8B GQA + the 1 GiB KV sanity anchor -------------------------------------
//
// Source: meta-llama/Llama-3.1-8B-Instruct config.json (public). 32 layers, 32 attn heads,
// 8 kv heads, hidden 4096 => head_dim 128. KV @ 8192 ctx, fp16, batch 1:
//
//	2 * 32 * 8 * 128 * 8192 * 2 bytes = 1,073,741,824 bytes = 1024 MiB = exactly 1 GiB.
func TestLlama31_KVSanity1GiB(t *testing.T) {
	cfg := loadConfig(t, "llama31-8b-config.json")

	// structural sanity
	if got := cfg.KVHeads(); got != 8 {
		t.Fatalf("kv heads = %d, want 8", got)
	}
	if got := cfg.HeadDimEff(); got != 128 {
		t.Fatalf("head_dim = %d, want 128 (4096/32)", got)
	}
	if !cfg.IsGQA() {
		t.Fatal("Llama-3.1-8B should be GQA (32 attn / 8 kv)")
	}
	if got := cfg.GQARatio(); got != 4.0 {
		t.Fatalf("GQA ratio = %v, want 4.0", got)
	}

	w := loadIndex(t, "llama31-8b-index.json")
	est, err := EstimateVRAM(cfg, w, Params{Ctx: 8192, Batch: 1, KVDtypeBytes: 2})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}

	// THE sanity anchor: KV must be exactly 1024 MiB (1 GiB).
	if est.KVCacheMiB != 1024.0 {
		t.Fatalf("KV @8K = %v MiB, want exactly 1024.0 (1 GiB)", est.KVCacheMiB)
	}
	// Weights from index total_size 16060522496 bytes = 15316.0 MiB.
	wantWeights := round1(float64(16060522496) / bytesPerMiB)
	if est.WeightsMiB != wantWeights {
		t.Fatalf("weights = %v MiB, want %v", est.WeightsMiB, wantWeights)
	}
	if est.Confidence != ConfidenceExact {
		t.Fatalf("confidence = %v, want exact (index size present)", est.Confidence)
	}
	if est.OverheadMiB != float64(vllmOverheadMiB) {
		t.Fatalf("overhead = %v, want %v", est.OverheadMiB, float64(vllmOverheadMiB))
	}
	wantTotal := round1(wantWeights + 1024.0 + float64(vllmOverheadMiB))
	if est.TotalMiB != wantTotal {
		t.Fatalf("total = %v, want %v", est.TotalMiB, wantTotal)
	}
	t.Logf("Llama-3.1-8B @8K: weights=%.1f KV=%.1f overhead=%.1f total=%.1f MiB (KV sanity = %.1f MiB = 1 GiB)",
		est.WeightsMiB, est.KVCacheMiB, est.OverheadMiB, est.TotalMiB, est.KVCacheMiB)
}

// --- (2) MHA model (Phi-1.5): no num_key_value_heads => n_kv == n_attn -----------------------
func TestPhi15_MHA(t *testing.T) {
	cfg := loadConfig(t, "phi1_5-mha-config.json")
	if cfg.NumKeyValueHeads != nil {
		t.Fatal("fixture should omit num_key_value_heads")
	}
	if got := cfg.KVHeads(); got != 32 {
		t.Fatalf("MHA kv heads = %d, want 32 (== num_attention_heads)", got)
	}
	if cfg.IsGQA() {
		t.Fatal("Phi-1.5 is MHA, not GQA")
	}
	if got := cfg.GQARatio(); got != 1.0 {
		t.Fatalf("MHA GQA ratio = %v, want 1.0", got)
	}
	est, err := EstimateVRAM(cfg, WeightsInfo{}, Params{Ctx: 2048, Batch: 1, KVDtypeBytes: 2})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if est.IsGQA {
		t.Fatal("estimate IsGQA should be false for MHA")
	}
	// KV @2048 fp16 batch1: 2(K/V) * 24 layers * 32 kv * 64 head_dim * 2048 ctx * 2 bytes
	// = 402,653,184 bytes = 384 MiB. head_dim = 2048/32 = 64.
	if got := cfg.HeadDimEff(); got != 64 {
		t.Fatalf("head_dim = %d, want 64", got)
	}
	if est.KVCacheMiB != 384.0 {
		t.Fatalf("Phi-1.5 KV @2K = %v MiB, want 384.0", est.KVCacheMiB)
	}
	// no index => approximate confidence (weights from param count).
	if est.Confidence != ConfidenceApproximate {
		t.Fatalf("confidence = %v, want approximate (no weights size)", est.Confidence)
	}
	t.Logf("Phi-1.5 (MHA) @2K: weights~%.1f KV=%.1f total~%.1f MiB, bpp=%.2f",
		est.WeightsMiB, est.KVCacheMiB, est.TotalMiB, est.BytesPerParam)
}

// --- (3) AWQ 4-bit: bytes_per_param ~0.5 + 10% scales ---------------------------------------
func TestAWQ_BytesPerParam(t *testing.T) {
	cfg := loadConfig(t, "llama2-7b-awq-config.json")
	if cfg.Quant() != QuantAWQ {
		t.Fatalf("quant = %q, want awq", cfg.Quant())
	}
	if got := bytesPerParam(cfg); got != 0.5 {
		t.Fatalf("AWQ bytes/param = %v, want 0.5", got)
	}
	// weights fallback path (no index): must be MUCH smaller than the fp16 equivalent.
	awqEst, err := EstimateVRAM(cfg, WeightsInfo{}, Params{Ctx: 4096, Batch: 1, KVDtypeBytes: 2})
	if err != nil {
		t.Fatalf("awq estimate: %v", err)
	}
	if awqEst.BytesPerParam != 0.5 {
		t.Fatalf("estimate bpp = %v, want 0.5", awqEst.BytesPerParam)
	}
	// Compare to the same architecture treated as fp16 (strip quant_config).
	fp16Cfg := cfg
	fp16Cfg.QuantizationConfig = nil
	fp16Est, err := EstimateVRAM(fp16Cfg, WeightsInfo{}, Params{Ctx: 4096, Batch: 1, KVDtypeBytes: 2})
	if err != nil {
		t.Fatalf("fp16 estimate: %v", err)
	}
	// AWQ weights ~ 0.5*1.10 / 2.0 = 0.275 of fp16 weights.
	ratio := awqEst.WeightsMiB / fp16Est.WeightsMiB
	if ratio > 0.35 || ratio < 0.2 {
		t.Fatalf("AWQ/fp16 weights ratio = %.3f, want ~0.275 (in [0.2,0.35])", ratio)
	}
	t.Logf("Llama-2-7B AWQ: weights~%.0f MiB vs fp16~%.0f MiB (ratio %.3f)",
		awqEst.WeightsMiB, fp16Est.WeightsMiB, ratio)
}

// --- (4) Missing-fields defensive tests -----------------------------------------------------
func TestDefensive_MissingKVHeads_DefaultsToMHA(t *testing.T) {
	cfg := ModelConfig{
		HiddenSize: 2048, NumHiddenLayers: 4, NumAttentionHeads: 16,
		IntermediateSize: 8192, VocabSize: 32000, MaxPositionEmbeds: 4096,
		TorchDtype: "float16",
		// NumKeyValueHeads deliberately nil.
	}
	if cfg.KVHeads() != 16 {
		t.Fatalf("missing kv heads => %d, want 16 (MHA fallback)", cfg.KVHeads())
	}
	est, err := EstimateVRAM(cfg, WeightsInfo{}, Params{Ctx: 1024})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	foundNote := false
	for _, n := range est.Notes {
		if len(n) > 0 && contains(n, "MHA") {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatalf("expected an MHA-fallback note, notes=%v", est.Notes)
	}
}

func TestDefensive_MissingHeadDim_Derived(t *testing.T) {
	cfg := ModelConfig{
		HiddenSize: 4096, NumHiddenLayers: 4, NumAttentionHeads: 32,
		IntermediateSize: 11008, VocabSize: 32000, MaxPositionEmbeds: 4096,
		// HeadDim nil => derive 4096/32 = 128.
	}
	if got := cfg.HeadDimEff(); got != 128 {
		t.Fatalf("derived head_dim = %d, want 128", got)
	}
}

func TestDefensive_ExplicitHeadDim_Preferred(t *testing.T) {
	// Gemma-2-9B: head_dim 256 != hidden/heads (3584/16 = 224). Must use explicit 256.
	cfg := loadConfig(t, "gemma2-9b-headdim-config.json")
	if got := cfg.HeadDimEff(); got != 256 {
		t.Fatalf("explicit head_dim = %d, want 256 (not derived 224)", got)
	}
	est, err := EstimateVRAM(cfg, WeightsInfo{}, Params{Ctx: 8192, Batch: 1, KVDtypeBytes: 2})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	// KV @8192: 2 * 42 * 8 * 256 * 8192 * 2 = 2,818,572,288 bytes = 2688 MiB.
	if est.KVCacheMiB != 2688.0 {
		t.Fatalf("Gemma-2-9B KV @8K = %v MiB, want 2688.0", est.KVCacheMiB)
	}
	// Confirm a note flags the explicit-head_dim deviation.
	found := false
	for _, n := range est.Notes {
		if contains(n, "explicit head_dim") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected explicit-head_dim note, notes=%v", est.Notes)
	}
}

func TestDefensive_BadJSON(t *testing.T) {
	_, err := ParseConfig([]byte("{not valid json"))
	if err == nil {
		t.Fatal("expected parse error on garbage JSON")
	}
}

func TestDefensive_ZeroCtxRejected(t *testing.T) {
	cfg := loadConfig(t, "phi1_5-mha-config.json")
	if _, err := EstimateVRAM(cfg, WeightsInfo{}, Params{Ctx: 0}); err == nil {
		t.Fatal("expected error for ctx=0")
	}
}

// --- (5) MaxContextThatFits monotonicity + cap ----------------------------------------------
func TestMaxContextThatFits_MonotonicAndCapped(t *testing.T) {
	cfg := loadConfig(t, "llama31-8b-config.json")
	w := loadIndex(t, "llama31-8b-index.json")

	// More free VRAM => never fewer tokens (monotonic non-decreasing).
	prev := -1
	for vram := 16000.0; vram <= 40000.0; vram += 2000.0 {
		mc := MaxContextThatFits(cfg, w, vram, 1, 2)
		if mc < prev {
			t.Fatalf("non-monotonic: vram=%.0f gave %d < prev %d", vram, mc, prev)
		}
		if mc%ctxAlign != 0 {
			t.Fatalf("max ctx %d not a multiple of %d", mc, ctxAlign)
		}
		prev = mc
	}

	// Cap at max_position_embeddings (131072). With an absurd amount of VRAM, must not exceed.
	capped := MaxContextThatFits(cfg, w, 1_000_000.0, 1, 2)
	if capped != cfg.MaxPositionEmbeds {
		t.Fatalf("with huge VRAM, max ctx = %d, want cap %d", capped, cfg.MaxPositionEmbeds)
	}

	// Too little VRAM to even load weights => 0.
	if got := MaxContextThatFits(cfg, w, 4000.0, 1, 2); got != 0 {
		t.Fatalf("weights don't fit in 4GB => max ctx should be 0, got %d", got)
	}
}

// --- (6) fits / doesn't-fit boundary against the reference GPU (16380 MiB) -------------------
func TestFitsBoundary_ReferenceGPU(t *testing.T) {
	cfg := loadConfig(t, "llama31-8b-config.json")
	w := loadIndex(t, "llama31-8b-index.json")

	// Llama-3.1-8B bf16 (~15316 MiB weights) + 1 GiB KV @8K + 2 GiB overhead ~= 18340 MiB.
	// That does NOT fit in 16380 MiB free under safety_factor. Correct verdict: wont_run.
	est8k, _ := EstimateVRAM(cfg, w, Params{Ctx: 8192, Batch: 1, KVDtypeBytes: 2})
	if est8k.FitsInVRAM(referenceGPUFreeMiB) {
		t.Fatalf("Llama-3.1-8B bf16 @8K (%.0f MiB) must NOT fit in %d MiB free (safety %.2f)",
			est8k.TotalMiB, referenceGPUFreeMiB, safetyFactor)
	}

	// The AWQ 4-bit variant (much smaller weights) SHOULD fit on the reference GPU at a modest
	// ctx. Use param-count fallback (no index) — still comfortably under budget.
	awq := loadConfig(t, "llama2-7b-awq-config.json")
	awqEst, _ := EstimateVRAM(awq, WeightsInfo{}, Params{Ctx: 4096, Batch: 1, KVDtypeBytes: 2})
	if !awqEst.FitsInVRAM(referenceGPUFreeMiB) {
		t.Fatalf("Llama-2-7B AWQ @4K (%.0f MiB) should fit in %d MiB free",
			awqEst.TotalMiB, referenceGPUFreeMiB)
	}
	t.Logf("boundary: llama3.1-8b bf16 @8K=%.0f MiB (no-fit), llama2-7b awq @4K=%.0f MiB (fit), free=%d MiB*%.2f=%.0f",
		est8k.TotalMiB, awqEst.TotalMiB, referenceGPUFreeMiB, safetyFactor, referenceGPUFreeMiB*safetyFactor)
}

// --- vLLM gpu-memory-utilization guidance ---------------------------------------------------
func TestGPUMemUtil_Guidance(t *testing.T) {
	awq := loadConfig(t, "llama2-7b-awq-config.json")
	est, _ := EstimateVRAM(awq, WeightsInfo{}, Params{Ctx: 4096, Batch: 1, KVDtypeBytes: 2})
	g := est.GPUMemUtil(referenceGPUTotalMiB)
	if !g.Fits {
		t.Fatalf("AWQ 7B should fit within util cap on %d MiB, guidance=%+v", referenceGPUTotalMiB, g)
	}
	if g.MinUtil <= 0 || g.MinUtil > 1 {
		t.Fatalf("MinUtil out of range: %v", g.MinUtil)
	}
	if g.RecommendUtil > vLLMMaxGPUMemUtil {
		t.Fatalf("RecommendUtil %v exceeds cap %v", g.RecommendUtil, vLLMMaxGPUMemUtil)
	}
	// A model that needs more than the ceiling of total => Fits=false.
	big := loadConfig(t, "llama31-8b-config.json")
	bigW := loadIndex(t, "llama31-8b-index.json")
	bigEst, _ := EstimateVRAM(big, bigW, Params{Ctx: 8192, Batch: 1, KVDtypeBytes: 2})
	bg := bigEst.GPUMemUtil(referenceGPUTotalMiB)
	if bg.Fits {
		t.Fatalf("llama3.1-8b bf16 @8K (%.0f MiB) must not fit within %.0f%% of %d MiB",
			bigEst.TotalMiB, vLLMMaxGPUMemUtil*100, referenceGPUTotalMiB)
	}
	t.Logf("AWQ guidance: min=%.3f recommend=%.2f fits=%v; big fits=%v", g.MinUtil, g.RecommendUtil, g.Fits, bg.Fits)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
