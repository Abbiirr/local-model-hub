package compat

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// evaluate_latency_test.go is the P6.1 <2s blocked-admission regression at the rule-engine
// level (fix-agent). It exercises the EXACT path that was the ~10s sink: a full Engine.Evaluate
// over a real LOCAL GGUF, which internally calls the estimator 4× (once at the requested ctx
// plus the three non-requested ctxCandidates via ggufMaxContext). Before the parse.go fix
// (UseMMap + SkipLargeMetadata on the local read), each parse cost ~2.3s size-independently, so
// one Evaluate ≈ 10s — and StartInstance runs this synchronously before residency Admit on the
// blocked-admission proxy path, blowing the <2s gate. Post-fix a whole Evaluate is well under 1s.
//
// Uses the 32B Q2_K reference file (an OVER-budget model at a 3000-MiB-ish free VRAM: the exact
// blocked-admission case the gate measures). Skips if the file is absent or in -short mode so a
// box without the models still tests green; the estimate CORRECTNESS is pinned separately by the
// gguf package's real-file tests (the fix is byte-identical to the default parse).
func TestRealFile_EvaluateUnder2s(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-file latency test in -short mode")
	}
	const path = `K:\projects\llm_models\models\qwen3-vl-32b-instruct-q2k\Qwen3-VL-32B-Instruct-Q2_K.gguf`
	if _, err := os.Stat(path); err != nil {
		t.Skipf("real model file not present, skipping: %s", path)
	}

	e := New()
	d := ModelDescriptor{
		ID: "qwen3-vl-32b-q2k", DisplayName: "Qwen3-VL-32B-Instruct-Q2_K",
		Format: FormatGGUF, Source: "local_path", Quantization: "q2_k",
		LocalPath: path, GGUFSource: gguf.LocalFile(path).WithModelKey("qwen3-vl-32b-q2k"),
	}
	// A tight free-VRAM snapshot so the 32B does NOT fit — the over-budget/blocked case whose
	// verdict the proxy turns into a fast 503. Availability: winget llama-server (Q2_K parses fine).
	sn := snap(3000, 16380, "8.9")
	av := availWith(winget)

	start := time.Now()
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192, Batch: 4}, sn, av)
	elapsed := time.Since(start)

	if v.Reason == "" {
		t.Fatal("verdict must always carry a reason (mandatory-reason gate)")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("Engine.Evaluate over a real 32B GGUF took %s (want < 2s). This is the P6.1 "+
			"blocked-admission gate: evaluateGGUF parses the file 4× (reqCtx + 3 ctxCandidates), so a "+
			"slow local parse blows the budget. Check parse.go localReadOptions (UseMMap + "+
			"SkipLargeMetadata) are still applied to the local parse path.", elapsed)
	}
	t.Logf("Evaluate(32B Q2_K, over-budget) = %s, status=%s est=%dMiB", elapsed, v.Status, v.EstimatedVRAMMiB)
}
