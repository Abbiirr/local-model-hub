package gguf

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// realfile_test.go pins wrapper correctness against real GGUF files on the
// reference box, and against a recorded remote HF metadata read. These read
// local files (or remote metadata) only — NO model loads, no VRAM use — so they
// are cheap. They are skipped under `go test -short` and skip individually if a
// file is absent (so the package still tests green on a box without the models).
//
// Recorded reference numbers come from research/phase-0-friction-log.md (P0.2).

const (
	realGLMOCR = `K:\projects\llm_models\models\glm-ocr\GLM-OCR-Q8_0.gguf`
	realTQ3    = `K:\projects\llm_models\models\Qwen3.6-27B-TQ3_4S.gguf`
)

func skipIfMissing(t *testing.T, path string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-file test in -short mode")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("real model file not present, skipping: %s", path)
	}
}

// GLM-OCR Q8_0 must PARSE and produce a plausible VRAM figure. The P0.2 CLI run
// recorded 1582 MiB @4K NONUMA (text-only, mmproj excluded); actual measured via
// NVML was ~1105 MiB — the parser number is intentionally conservative-over,
// which is the correct direction (a false "fits" would OOM). The library called
// directly (this wrapper) yields ~1331 MiB @4K; we assert it lands in a band that
// straddles the measured value and the CLI figure and stays conservative.
func TestRealFile_GLMOCR_Parses(t *testing.T) {
	skipIfMissing(t, realGLMOCR)

	res, err := Estimate(context.Background(), LocalFile(realGLMOCR), Options{
		ContextLen: 4096, FlashAttention: true, NGL: -1, ParallelSeq: 1,
	})
	if err != nil {
		t.Fatalf("Estimate(GLM-OCR): %v", err)
	}
	if res.Confidence != ConfidenceApproximate {
		t.Fatalf("Confidence = %q, want approximate (GLM-OCR is a standard Q8_0, must parse)", res.Confidence)
	}
	if res.ParserVersion != ParserVersion {
		t.Errorf("ParserVersion = %q, want %q", res.ParserVersion, ParserVersion)
	}
	if res.Architecture == "" {
		t.Errorf("Architecture empty; expected e.g. glm4")
	}
	// Band: comfortably above the measured ~1105 MiB (conservative) and not wildly
	// above the CLI's 1582 MiB. Library direct call ≈ 1331 MiB.
	if res.VRAMMiB < 1000 || res.VRAMMiB > 1800 {
		t.Errorf("GLM-OCR VRAM = %d MiB @4K, want within [1000,1800] "+
			"(measured ~1105, CLI recorded 1582, library ~1331)", res.VRAMMiB)
	}
	t.Logf("GLM-OCR @4K: VRAM=%d MiB RAM=%d MiB arch=%s layers=%d",
		res.VRAMMiB, res.RAMMiB, res.Architecture, res.Layers)
}

// Qwen3.6-27B-TQ3_4S must hit the FALLBACK path (gguf-parser-go cannot parse
// TQ3_* — GGMLType(46)) WITHOUT an error, and yield a plausible ~13-14 GB
// weights-dominated number.
func TestRealFile_TQ3_Fallback(t *testing.T) {
	skipIfMissing(t, realTQ3)

	res, err := Estimate(context.Background(), LocalFile(realTQ3), Options{
		ContextLen: 4096, FlashAttention: true, NGL: -1, ParallelSeq: 1,
		KVCacheType: KVCacheQ4_0,
	})
	if err != nil {
		t.Fatalf("Estimate(TQ3) must fall back, not error: %v", err)
	}
	if res.Confidence != ConfidenceFallback {
		t.Fatalf("Confidence = %q, want fallback (TQ3 is unparseable)", res.Confidence)
	}
	// File is ~13.95 GB; weights ≈ size×1.05 → ~14.6 GB; plus KV + overhead.
	// Assert the number sits in a plausible 13-16 GB VRAM band (MiB).
	if res.VRAMMiB < 13000 || res.VRAMMiB > 16500 {
		t.Errorf("TQ3 fallback VRAM = %d MiB, want within [13000,16500] (~13.95 GB file)", res.VRAMMiB)
	}
	joined := strings.Join(res.Notes, " | ")
	if !strings.Contains(joined, "FALLBACK") || !strings.Contains(joined, "GGMLType(46)") {
		t.Errorf("TQ3 notes must explain the fallback + GGMLType(46): %q", joined)
	}
	t.Logf("TQ3 fallback: VRAM=%d MiB (ctxVRAM=%d) arch=%s layers=%d",
		res.VRAMMiB, res.CtxVRAMMiB, res.Architecture, res.Layers)
	t.Logf("TQ3 notes: %s", joined)
}

// (e) OPTIONAL remote HF metadata read, guarded by both -short and a network env
// flag (LOCALHUB_TEST_NETWORK=1). Records against P0.2: bartowski Llama-3.2-1B
// GGUF was 1798 MiB @8K. Tolerates a CI-less / offline environment by skipping.
func TestRealFile_RemoteHF_Metadata(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping remote HF test in -short mode")
	}
	if os.Getenv("LOCALHUB_TEST_NETWORK") != "1" {
		t.Skip("set LOCALHUB_TEST_NETWORK=1 to run the remote HF metadata test")
	}

	res, err := Estimate(context.Background(),
		HFRepo("bartowski/Llama-3.2-1B-Instruct-GGUF", "Llama-3.2-1B-Instruct-Q4_K_M.gguf", ""),
		Options{ContextLen: 8192, FlashAttention: true, NGL: -1, ParallelSeq: 1, RemoteTimeout: 20 * time.Second},
	)
	if err != nil {
		t.Skipf("remote HF read failed (network/rate-limit tolerated): %v", err)
	}
	if res.Confidence != ConfidenceApproximate {
		t.Errorf("Confidence = %q, want approximate", res.Confidence)
	}
	// P0.2 recorded 1798 MiB @8K; allow a generous band for library/version drift.
	if res.VRAMMiB < 1200 || res.VRAMMiB > 2600 {
		t.Errorf("remote Llama-3.2-1B VRAM = %d MiB @8K, want ~1798 (band [1200,2600])", res.VRAMMiB)
	}
	t.Logf("remote Llama-3.2-1B @8K: VRAM=%d MiB arch=%s", res.VRAMMiB, res.Architecture)
}
