package safetensors

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Property tests: simple loops, no framework (task spec). Each asserts an invariant of the
// formula that must hold across inputs, not just for one fixture.

// KV scales linearly with ctx: KV(k*ctx) == k*KV(ctx) (exactly, since KV is linear in ctx).
func TestProperty_KVLinearInCtx(t *testing.T) {
	cfg := loadConfig(t, "llama31-8b-config.json")
	w := loadIndex(t, "llama31-8b-index.json")

	base, err := EstimateVRAM(cfg, w, Params{Ctx: 1024, Batch: 1, KVDtypeBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []int{2, 4, 8, 16, 32} {
		est, err := EstimateVRAM(cfg, w, Params{Ctx: 1024 * k, Batch: 1, KVDtypeBytes: 2})
		if err != nil {
			t.Fatal(err)
		}
		want := round1(base.KVCacheMiB * float64(k))
		if est.KVCacheMiB != want {
			t.Fatalf("KV not linear: ctx=%d KV=%v, want %v (%d x base %v)",
				1024*k, est.KVCacheMiB, want, k, base.KVCacheMiB)
		}
		// weights + overhead must be ctx-invariant.
		if est.WeightsMiB != base.WeightsMiB || est.OverheadMiB != base.OverheadMiB {
			t.Fatalf("weights/overhead changed with ctx: %+v vs %+v", est, base)
		}
	}
}

// KV also scales linearly with batch.
func TestProperty_KVLinearInBatch(t *testing.T) {
	cfg := loadConfig(t, "llama31-8b-config.json")
	w := loadIndex(t, "llama31-8b-index.json")
	base, _ := EstimateVRAM(cfg, w, Params{Ctx: 4096, Batch: 1, KVDtypeBytes: 2})
	for _, b := range []int{2, 4, 8} {
		est, _ := EstimateVRAM(cfg, w, Params{Ctx: 4096, Batch: b, KVDtypeBytes: 2})
		want := round1(base.KVCacheMiB * float64(b))
		if est.KVCacheMiB != want {
			t.Fatalf("KV not linear in batch: batch=%d KV=%v want %v", b, est.KVCacheMiB, want)
		}
	}
}

// GQA KV < MHA KV for the same dims: turning a GQA config into MHA (kv==attn) must strictly
// increase KV by exactly the GQA ratio.
func TestProperty_GQAKVLessThanMHA(t *testing.T) {
	gqa := loadConfig(t, "llama31-8b-config.json") // 8 kv / 32 attn
	w := loadIndex(t, "llama31-8b-index.json")

	mha := gqa
	attn := gqa.NumAttentionHeads
	mha.NumKeyValueHeads = &attn // force MHA

	gqaEst, _ := EstimateVRAM(gqa, w, Params{Ctx: 8192, Batch: 1, KVDtypeBytes: 2})
	mhaEst, _ := EstimateVRAM(mha, w, Params{Ctx: 8192, Batch: 1, KVDtypeBytes: 2})

	if !(gqaEst.KVCacheMiB < mhaEst.KVCacheMiB) {
		t.Fatalf("GQA KV %.1f should be < MHA KV %.1f", gqaEst.KVCacheMiB, mhaEst.KVCacheMiB)
	}
	// The ratio is exactly the GQA ratio (4x here).
	ratio := mhaEst.KVCacheMiB / gqaEst.KVCacheMiB
	if round1(ratio) != round1(gqa.GQARatio()) {
		t.Fatalf("MHA/GQA KV ratio %.2f != GQA ratio %.2f", ratio, gqa.GQARatio())
	}
}

// Quantized weights < fp16 weights for the same architecture.
func TestProperty_QuantWeightsLessThanFP16(t *testing.T) {
	awq := loadConfig(t, "llama2-7b-awq-config.json")
	fp16 := awq
	fp16.QuantizationConfig = nil // same dims, no quant

	awqEst, _ := EstimateVRAM(awq, WeightsInfo{}, Params{Ctx: 2048, Batch: 1})
	fp16Est, _ := EstimateVRAM(fp16, WeightsInfo{}, Params{Ctx: 2048, Batch: 1})
	if !(awqEst.WeightsMiB < fp16Est.WeightsMiB) {
		t.Fatalf("AWQ weights %.0f should be < fp16 %.0f", awqEst.WeightsMiB, fp16Est.WeightsMiB)
	}

	// fp8 also < fp16.
	fp8 := fp16
	fp8.QuantizationConfig = &quantizationConfig{QuantMethod: "fp8"}
	fp8Est, _ := EstimateVRAM(fp8, WeightsInfo{}, Params{Ctx: 2048, Batch: 1})
	if !(fp8Est.WeightsMiB < fp16Est.WeightsMiB) {
		t.Fatalf("fp8 weights %.0f should be < fp16 %.0f", fp8Est.WeightsMiB, fp16Est.WeightsMiB)
	}
	if !(awqEst.WeightsMiB < fp8Est.WeightsMiB) {
		t.Fatalf("awq(4bit) %.0f should be < fp8 %.0f", awqEst.WeightsMiB, fp8Est.WeightsMiB)
	}
}

// fp8 KV cache is exactly half the fp16 KV cache.
func TestProperty_FP8KVHalfOfFP16(t *testing.T) {
	cfg := loadConfig(t, "llama31-8b-config.json")
	w := loadIndex(t, "llama31-8b-index.json")
	fp16, _ := EstimateVRAM(cfg, w, Params{Ctx: 8192, Batch: 1, KVDtypeBytes: 2})
	fp8, _ := EstimateVRAM(cfg, w, Params{Ctx: 8192, Batch: 1, KVDtypeBytes: 1})
	if fp8.KVCacheMiB != round1(fp16.KVCacheMiB/2) {
		t.Fatalf("fp8 KV %.1f != half of fp16 KV %.1f", fp8.KVCacheMiB, fp16.KVCacheMiB)
	}
}

// --- Weights source parsing ------------------------------------------------------------------

func TestWeights_ShardSum(t *testing.T) {
	w, ok := WeightsFromShardSizes([]int64{1000, 2000, 3000})
	if !ok || w.TotalBytes != 6000 || w.Source != WeightsFromShards {
		t.Fatalf("shard sum wrong: %+v ok=%v", w, ok)
	}
	if _, ok := WeightsFromShardSizes(nil); ok {
		t.Fatal("empty shard list should be ok=false")
	}
}

func TestWeights_IndexParse(t *testing.T) {
	w := loadIndex(t, "llama31-8b-index.json")
	if w.TotalBytes != 16060522496 || w.Source != WeightsFromIndex {
		t.Fatalf("index parse wrong: %+v", w)
	}
	if _, ok := ParseIndexTotalSize([]byte(`{"metadata":{}}`)); ok {
		t.Fatal("index without total_size should be ok=false")
	}
}

// Local safetensors header parse (optional path): synthesize a tiny valid file (header only,
// no tensor bodies needed for the size calc since we read data_offsets) and verify the summed
// data size.
func TestWeights_LocalHeaderParse(t *testing.T) {
	dir := t.TempDir()
	// Two tensors: one 0..1024, one 1024..4096 => max end = 4096 bytes.
	header := map[string]any{
		"__metadata__": map[string]string{"format": "pt"},
		"model.a":      map[string]any{"dtype": "F16", "shape": []int{512}, "data_offsets": []int{0, 1024}},
		"model.b":      map[string]any{"dtype": "F16", "shape": []int{1536}, "data_offsets": []int{1024, 4096}},
	}
	hbytes, _ := json.Marshal(header)
	path := filepath.Join(dir, "model.safetensors")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(hbytes)))
	f.Write(lenBuf[:])
	f.Write(hbytes)
	// We intentionally do NOT write the 4096 tensor bytes — the parser only reads the header.
	f.Close()

	w, ok := SumLocalSafetensorsHeaders([]string{path})
	if !ok {
		t.Fatal("expected header parse to succeed")
	}
	if w.TotalBytes != 4096 || w.Source != WeightsFromHeader {
		t.Fatalf("header size = %+v, want 4096 bytes from header", w)
	}
}

func TestWeights_LocalHeader_BadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.safetensors")
	os.WriteFile(path, []byte("too short"), 0o644)
	if _, ok := SumLocalSafetensorsHeaders([]string{path}); ok {
		t.Fatal("garbage file should not parse")
	}
}
