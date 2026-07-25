package gguf

import (
	"context"
	"strings"
	"testing"
)

// (a) Synthetic-GGUF header reader test — validates the raw KV metadata reader
// used to feed the fallback. (The estimate happy-path is validated by the
// real-file tests below, because gguf-parser-go's estimator panics on a
// tensor-less synthetic file — it needs real tensor structure.)
func TestReadGGUFHeaderMetadata_Synthetic(t *testing.T) {
	m := syntheticModel{
		arch: "llama", blockCount: 4, embedding: 64,
		headCount: 8, headCountKV: 2, contextLength: 4096,
	}
	path := writeSyntheticGGUF(t, "synth.gguf", buildGGUF(m, false, 0))

	hdr, err := readGGUFHeaderMetadata(path)
	if err != nil {
		t.Fatalf("readGGUFHeaderMetadata: %v", err)
	}
	if hdr.architecture != "llama" {
		t.Errorf("arch = %q, want llama", hdr.architecture)
	}
	if hdr.blockCount != 4 || hdr.embeddingLength != 64 || hdr.headCount != 8 || hdr.headCountKV != 2 {
		t.Errorf("meta = %+v, want block=4 embd=64 head=8 headKV=2", hdr)
	}
	if hdr.contextLength != 4096 {
		t.Errorf("ctxLen = %d, want 4096", hdr.contextLength)
	}
	if got := hdr.headDim(); got != 8 { // 64/8
		t.Errorf("headDim = %d, want 8", got)
	}
}

// MHA fallback: when head_count_kv is absent, KV must use head_count.
func TestReadGGUFHeaderMetadata_MHA(t *testing.T) {
	m := syntheticModel{arch: "gptmha", blockCount: 2, embedding: 32, headCount: 4, headCountKV: 0}
	path := writeSyntheticGGUF(t, "mha.gguf", buildGGUF(m, false, 0))
	hdr, err := readGGUFHeaderMetadata(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if hdr.headCountKV != 0 {
		t.Errorf("headCountKV = %d, want 0 (absent)", hdr.headCountKV)
	}
	// kvCacheBytes must substitute head_count for the KV head count.
	got := kvCacheBytes(hdr, 128, 1, 2)
	// 2 * layers(2) * nKV(=head_count 4) * head_dim(32/4=8) * ctx(128) * par(1) * 2
	want := uint64(2 * 2 * 4 * 8 * 128 * 1 * 2)
	if got != want {
		t.Errorf("kvCacheBytes(MHA) = %d, want %d", got, want)
	}
}

// (b) Fallback-path test: a fake TQ3-like unknown-type GGUF (GGMLType 46) must
// NOT error out of Estimate — it must return a fallback estimate with a
// plausible weights term and an explanatory note.
func TestEstimate_FallbackOnUnknownType(t *testing.T) {
	m := syntheticModel{
		arch: "llama", blockCount: 8, embedding: 128,
		headCount: 8, headCountKV: 2, contextLength: 8192,
	}
	// tensor=true with ggmlType 46 => gguf-parser-go aborts with GGMLType(46).
	data := buildGGUF(m, true, 46)
	path := writeSyntheticGGUF(t, "tq3fake.gguf", data)

	res, err := Estimate(context.Background(), LocalFile(path), Options{
		ContextLen: 4096, FlashAttention: true, NGL: -1, ParallelSeq: 1,
	})
	if err != nil {
		t.Fatalf("Estimate returned error on unparseable file (must fall back, not error): %v", err)
	}
	if res.Confidence != ConfidenceFallback {
		t.Fatalf("Confidence = %q, want fallback", res.Confidence)
	}
	if res.VRAMMiB <= 0 {
		t.Errorf("VRAMMiB = %d, want > 0", res.VRAMMiB)
	}
	// Weights term ≈ file_size × 1.05. File is tiny, so VRAM is dominated by the
	// fixed overhead + small KV; assert the fixed overhead is included.
	if res.VRAMMiB < fallbackFixedOverheadMiB {
		t.Errorf("VRAMMiB = %d, want >= fixed overhead %d", res.VRAMMiB, fallbackFixedOverheadMiB)
	}
	if res.CtxVRAMMiB == 0 {
		t.Errorf("CtxVRAMMiB = 0, want a KV term from header metadata")
	}
	if res.Architecture != "llama" {
		t.Errorf("Architecture = %q, want llama (from header)", res.Architecture)
	}
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "FALLBACK") || !strings.Contains(joined, "GGMLType(46)") {
		t.Errorf("Notes missing fallback/why explanation: %q", joined)
	}
	if !strings.Contains(joined, "n_head_kv=2") {
		t.Errorf("Notes should record the header inputs used: %q", joined)
	}
}

// Fallback with an unreadable header (bad magic) => file-size-only formula with
// a loud note, still no error.
func TestEstimate_FallbackFileSizeOnly(t *testing.T) {
	// A file that is NOT a valid GGUF (bad magic) but non-empty: gguf-parser-go
	// errors, then our header reader also errors (bad magic), so we hit the
	// file-size-only branch.
	path := writeSyntheticGGUF(t, "garbage.gguf", []byte("NOTGGUFxxxxxxxxxxxxxxxxxxxxxxxxxx"))

	res, err := Estimate(context.Background(), LocalFile(path), Options{ContextLen: 4096})
	if err != nil {
		t.Fatalf("Estimate errored on garbage local file (want file-size fallback): %v", err)
	}
	if res.Confidence != ConfidenceFallback {
		t.Fatalf("Confidence = %q, want fallback", res.Confidence)
	}
	if res.VRAMMiB < fallbackFixedOverheadMiB {
		t.Errorf("VRAMMiB = %d, want >= fixed overhead", res.VRAMMiB)
	}
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "LOUD NOTE") {
		t.Errorf("file-size-only fallback must carry a loud note: %q", joined)
	}
}

// (c) Calibration-store override test: an observed peak beats any estimate and
// yields Confidence=exact — even when the underlying file would otherwise parse
// or fall back.
type fakeCalibration struct {
	peaks map[string]int
}

func (f fakeCalibration) ObservedPeak(key string) (int, bool) {
	v, ok := f.peaks[key]
	return v, ok
}

func TestEstimate_CalibrationOverride(t *testing.T) {
	m := syntheticModel{arch: "llama", blockCount: 8, embedding: 128, headCount: 8, headCountKV: 2}
	path := writeSyntheticGGUF(t, "cal.gguf", buildGGUF(m, true, 46)) // would otherwise fall back
	key := "qwen3.6-27b-tq3_4s"

	store := fakeCalibration{peaks: map[string]int{key: 13312}}
	res, err := EstimateWithCalibration(
		context.Background(),
		LocalFile(path).WithModelKey(key),
		Options{ContextLen: 4096},
		store,
	)
	if err != nil {
		t.Fatalf("EstimateWithCalibration: %v", err)
	}
	if res.Confidence != ConfidenceExact {
		t.Fatalf("Confidence = %q, want exact", res.Confidence)
	}
	if res.VRAMMiB != 13312 {
		t.Errorf("VRAMMiB = %d, want 13312 (observed peak)", res.VRAMMiB)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "measured") {
		t.Errorf("exact note should mention measured peak: %v", res.Notes)
	}

	// No observed peak for this key => falls through to normal path (fallback here).
	res2, err := EstimateWithCalibration(
		context.Background(),
		LocalFile(path).WithModelKey("unknown-key"),
		Options{ContextLen: 4096},
		store,
	)
	if err != nil {
		t.Fatalf("EstimateWithCalibration (miss): %v", err)
	}
	if res2.Confidence != ConfidenceFallback {
		t.Errorf("calibration miss Confidence = %q, want fallback", res2.Confidence)
	}
}

// Input validation: bad KV type and empty source are surfaced as errors on the
// PARSE path (a valid parseable file is required to reach kv validation; here we
// assert the empty-source guard and the KV mapping helper directly).
func TestEstimate_EmptySource(t *testing.T) {
	if _, err := Estimate(context.Background(), Source{}, Options{}); err == nil {
		t.Error("expected error for empty source")
	}
	if _, err := Estimate(context.Background(), LocalFile(""), Options{}); err == nil {
		t.Error("expected error for empty local path")
	}
}

func TestKVGGMLType(t *testing.T) {
	for _, tc := range []struct {
		in KVCacheType
		ok bool
	}{
		{KVCacheF16, true}, {KVCacheQ8_0, true}, {KVCacheQ4_0, true}, {"bogus", false},
	} {
		_, err := kvGGMLType(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("kvGGMLType(%q): err=%v, wantOK=%v", tc.in, err, tc.ok)
		}
	}
}
