package gguf

import (
	"context"
	"os"
	"testing"

	ggufparser "github.com/gpustack/gguf-parser-go"
)

// --- fixture helpers: build a minimal in-memory GGUF metadata set ---

func scalarU32(key string, v uint32) ggufparser.GGUFMetadataKV {
	return ggufparser.GGUFMetadataKV{Key: key, ValueType: ggufparser.GGUFMetadataValueTypeUint32, Value: v}
}

func strKV(key, v string) ggufparser.GGUFMetadataKV {
	return ggufparser.GGUFMetadataKV{Key: key, ValueType: ggufparser.GGUFMetadataValueTypeString, Value: v}
}

func int32ArrayKV(key string, vals ...int32) ggufparser.GGUFMetadataKV {
	arr := make([]any, len(vals))
	for i, v := range vals {
		arr[i] = v
	}
	return ggufparser.GGUFMetadataKV{
		Key:       key,
		ValueType: ggufparser.GGUFMetadataValueTypeArray,
		Value: ggufparser.GGUFMetadataKVArrayValue{
			Type: ggufparser.GGUFMetadataValueTypeInt32, Len: uint64(len(vals)), Array: arr,
		},
	}
}

func boolArrayKV(key string, vals ...bool) ggufparser.GGUFMetadataKV {
	arr := make([]any, len(vals))
	for i, v := range vals {
		arr[i] = v
	}
	return ggufparser.GGUFMetadataKV{
		Key:       key,
		ValueType: ggufparser.GGUFMetadataValueTypeArray,
		Value: ggufparser.GGUFMetadataKVArrayValue{
			Type: ggufparser.GGUFMetadataValueTypeBool, Len: uint64(len(vals)), Array: arr,
		},
	}
}

func fakeGemma4(extra ...ggufparser.GGUFMetadataKV) *ggufparser.GGUFFile {
	kvs := ggufparser.GGUFMetadataKVs{
		strKV("general.architecture", "gemma4"),
		scalarU32("gemma4.block_count", 6),
		scalarU32("gemma4.context_length", 262144),
		scalarU32("gemma4.attention.key_length", 512),
		scalarU32("gemma4.attention.value_length", 512),
		scalarU32("gemma4.attention.key_length_swa", 256),
		scalarU32("gemma4.attention.value_length_swa", 256),
		scalarU32("gemma4.attention.sliding_window", 1024),
		// 5 sliding (true) + 1 global (false); KV heads 8 on sliding, 1 on global.
		boolArrayKV("gemma4.attention.sliding_window_pattern", true, true, true, true, true, false),
		int32ArrayKV("gemma4.attention.head_count_kv", 8, 8, 8, 8, 8, 1),
	}
	kvs = append(kvs, extra...)
	return &ggufparser.GGUFFile{Header: ggufparser.GGUFHeader{MetadataKVCount: uint64(len(kvs)), MetadataKV: kvs}}
}

// TestISWAKVFloorBytes_Gemma4 pins the iSWA KV math against a hand-computed value.
// 6 layers: 5 sliding (head_count_kv=8, head_dim 256, window-capped to n_swa+batch
// = 1024+2048 = 3072 tokens) + 1 global (head_count_kv=1, head_dim 512, full ctx).
// At ctx 4096, f16 (2 B/elem):
//
//	sliding: (256+256)*8 * 2 * 3072 = 25,165,824 B/layer * 5 = 125,829,120
//	global : (512+512)*1 * 2 * 4096 =  8,388,608 B/layer * 1 =   8,388,608
//	total  = 134,217,728 B = exactly 128 MiB
func TestISWAKVFloorBytes_Gemma4(t *testing.T) {
	f := fakeGemma4()
	bytes, nSWA, nGlobal, _, ok := iswaKVFloorBytes(f, 4096, KVCacheF16)
	if !ok {
		t.Fatal("iswaKVFloorBytes: expected ok=true for a gemma4 fixture")
	}
	if nSWA != 5 || nGlobal != 1 {
		t.Errorf("layer split: got %d sliding + %d global, want 5 + 1", nSWA, nGlobal)
	}
	const want = uint64(134_217_728)
	if bytes != want {
		t.Errorf("iSWA KV floor: got %d B, want %d B (128 MiB)", bytes, want)
	}
}

// TestISWAKVFloorBytes_GlobalScalesWithCtx confirms only the GLOBAL layers grow
// with context (sliding layers are window-capped): doubling ctx adds one global
// layer's worth of KV, not the whole cache.
func TestISWAKVFloorBytes_GlobalScalesWithCtx(t *testing.T) {
	f := fakeGemma4()
	at4k, _, _, _, _ := iswaKVFloorBytes(f, 4096, KVCacheF16)
	at8k, _, _, _, _ := iswaKVFloorBytes(f, 8192, KVCacheF16)
	// One global layer: (512+512)*1*2 = 2048 B/token; extra 4096 tokens = 8,388,608 B.
	if got := at8k - at4k; got != 8_388_608 {
		t.Errorf("ctx 4k->8k KV delta: got %d B, want 8,388,608 B (one global layer * 4096 tokens)", got)
	}
}

// TestISWAKVFloorBytes_NonSWA: a model with no sliding_window key must be left
// alone (ok=false) — the correction only touches genuine SWA models.
func TestISWAKVFloorBytes_NonSWA(t *testing.T) {
	kvs := ggufparser.GGUFMetadataKVs{
		strKV("general.architecture", "llama"),
		scalarU32("llama.block_count", 32),
		scalarU32("llama.attention.key_length", 128),
		scalarU32("llama.attention.value_length", 128),
		scalarU32("llama.attention.head_count_kv", 8),
	}
	f := &ggufparser.GGUFFile{Header: ggufparser.GGUFHeader{MetadataKV: kvs}}
	if _, _, _, _, ok := iswaKVFloorBytes(f, 4096, KVCacheF16); ok {
		t.Error("iswaKVFloorBytes: non-SWA model must return ok=false")
	}
}

// TestISWAKVFloorBytes_QuantizedIsSmaller sanity-checks the KV dtype scaling: a
// q4_0 cache floor must be well below the f16 floor (fewer bytes per element).
func TestISWAKVFloorBytes_QuantizedIsSmaller(t *testing.T) {
	f := fakeGemma4()
	f16, _, _, _, _ := iswaKVFloorBytes(f, 4096, KVCacheF16)
	q4, _, _, _, _ := iswaKVFloorBytes(f, 4096, KVCacheQ4_0)
	if q4 >= f16 {
		t.Errorf("q4_0 floor (%d) should be smaller than f16 floor (%d)", q4, f16)
	}
}

// realGemma4 is the on-disk gemma-4-12B GGUF (iSWA). The real-file test is skipped
// when absent so the suite stays green on boxes without the model.
const realGemma4 = `K:\projects\llm_models\models\yuxinlu1__gemma-4-12B-agentic-fable5-composer2.5-v2-3.5x-tau2-GGUF\gemma4-v2-Q4_K_M.gguf`

// TestRealFile_Gemma4_ISWACorrection proves the end-to-end wrapper behaviour on the
// real model: gguf-parser-go prices its KV at ~0 under SkipLargeMetadata, and our
// correction adds a conservative iSWA KV floor (~1024 MiB @4K) that grows only on
// the global layers with context. This is the correctness fix for the false-fit
// OOM risk (CLAUDE.md).
func TestRealFile_Gemma4_ISWACorrection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-file test in -short mode")
	}
	if _, err := os.Stat(realGemma4); err != nil {
		t.Skipf("gemma-4 model not present, skipping: %s", realGemma4)
	}
	src := LocalFile(realGemma4)

	r4, err := Estimate(context.Background(), src, Options{ContextLen: 4096, FlashAttention: true, NGL: -1})
	if err != nil {
		t.Fatalf("estimate @4K: %v", err)
	}
	if r4.Architecture != "gemma4" {
		t.Fatalf("architecture = %q, want gemma4", r4.Architecture)
	}
	// Correction must have fired: CtxVRAMMiB (the iSWA KV floor) is ~1 GiB, not 0.
	if r4.CtxVRAMMiB < 800 {
		t.Errorf("@4K iSWA KV floor = %d MiB, want ~1024 (correction should have fired)", r4.CtxVRAMMiB)
	}
	// A SWA-correction note must be present (verdict transparency, CLAUDE.md).
	var hasNote bool
	for _, n := range r4.Notes {
		if len(n) > 0 && (contains(n, "SWA correction")) {
			hasNote = true
		}
	}
	if !hasNote {
		t.Errorf("@4K notes should include an 'SWA correction' entry; got %v", r4.Notes)
	}

	r32, err := Estimate(context.Background(), src, Options{ContextLen: 32768, FlashAttention: true, NGL: -1})
	if err != nil {
		t.Fatalf("estimate @32K: %v", err)
	}
	// Global-layer KV grows with ctx, so 32K VRAM must exceed 4K VRAM.
	if r32.VRAMMiB <= r4.VRAMMiB {
		t.Errorf("@32K VRAM (%d) should exceed @4K VRAM (%d) — global KV grows with ctx", r32.VRAMMiB, r4.VRAMMiB)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
