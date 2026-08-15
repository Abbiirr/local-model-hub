package compat

import (
	"context"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// These selection tests are the M1.GATE + M3.GATE regression suite. They prove:
//   - M3.7/A: HasCUDA is no longer a selection criterion. A binary reporting has_cuda=false
//     wins when configured first and no capability requires the fork (the exact b10106 case
//     that used to lose to the has_cuda TQ3 fork and then die on Qwen3.6's hybrid arch).
//   - M3.6: selection is deterministic — required capabilities first, configured order second.
//     TQ3 models still resolve to the has_tq3 fork.
//
// b10106's real detected state after M3.7/B is HasCUDA=true (CUDA inferred from the co-located
// ggml-cuda.dll). We also model the PRE-fix has_cuda=false state to prove selection is
// CUDA-agnostic regardless.
var (
	modernBuild = LlamaBinary{
		Path: "b10106", Detail: "CUDA build", Detected: true,
		HasTQ3: false, HasFlashAttn: true, HasCUDA: true, SupportsKVQuant: true,
	}
	// modernBuildNoCuda models b10106's PRE-fix detected state (has_cuda=false). Used to prove
	// the OLD "prefer has_cuda" heuristic is gone: even a has_cuda=false binary wins when first.
	modernBuildNoCuda = LlamaBinary{
		Path: "b10106", Detail: "b10106", Detected: true,
		HasTQ3: false, HasFlashAttn: true, HasCUDA: false, SupportsKVQuant: true,
	}
)

func TestBestLlamaBinary_ConfigOrderBeatsHasCUDA(t *testing.T) {
	// THE M1.GATE regression: b10106 (has_cuda=FALSE, the pre-fix detection state) configured
	// FIRST, the has_cuda TQ3 fork SECOND, no capability requirement. OLD code returned the
	// fork (preferred has_cuda) → it then crashed on Qwen3.6. NEW code returns b10106 by
	// configured order.
	for _, first := range []LlamaBinary{modernBuild, modernBuildNoCuda} {
		avail := EngineAvailability{LlamaBinaries: []LlamaBinary{first, cudaFork}}
		got, ok := avail.bestLlamaBinary(BinaryRequirements{})
		if !ok {
			t.Fatalf("expected a selection, got none (first.HasCUDA=%v)", first.HasCUDA)
		}
		if got.Path != "b10106" {
			t.Errorf("config-order selection: got %q, want b10106 (HasCUDA must not bias selection). first.HasCUDA=%v",
				got.Path, first.HasCUDA)
		}
	}
}

func TestBestLlamaBinary_HasCUDAListedLaterIsNotPreferred(t *testing.T) {
	// A non-CUDA binary configured first wins over a CUDA binary configured second, for a
	// model with no special requirements. Proves has_cuda is never a tie-break.
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{winget, cudaFork}}
	got, ok := avail.bestLlamaBinary(BinaryRequirements{})
	if !ok || got.Path != winget.Path {
		t.Fatalf("expected first-configured %q, got %q (ok=%v) — has_cuda must not override order",
			winget.Path, got.Path, ok)
	}
}

func TestBestLlamaBinary_TQ3RequirementPicksFork(t *testing.T) {
	// A TQ3_* model REQUIRES has_tq3. Even with b10106 configured first, selection must skip
	// it (no has_tq3) and resolve to the fork. This is the "TQ3 models still resolve to the
	// fork" half of M1.GATE — capability requirement beats configured order.
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{modernBuild, cudaFork, winget}}
	got, ok := avail.bestLlamaBinary(BinaryRequirements{TQ3: true})
	if !ok {
		t.Fatal("expected the fork to be selected for a TQ3 model, got none")
	}
	if !got.HasTQ3 {
		t.Errorf("TQ3 requirement must select a has_tq3 binary, got %q (has_tq3=false)", got.Path)
	}
	if got.Path != cudaFork.Path {
		t.Errorf("expected the fork %q, got %q", cudaFork.Path, got.Path)
	}
}

func TestBestLlamaBinary_TQ3RequirementNoForkAvailable(t *testing.T) {
	// A TQ3 model with no has_tq3 binary available → (zero, false). The caller turns this into
	// a wont_run verdict naming the fork (see evaluateGGUF / gguf_path.go).
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{modernBuild, winget}}
	if _, ok := avail.bestLlamaBinary(BinaryRequirements{TQ3: true}); ok {
		t.Error("TQ3 requirement with no has_tq3 binary must return (zero,false)")
	}
}

func TestBestLlamaBinary_SkipsNotDetected(t *testing.T) {
	// A not-detected candidate is never selected even if configured first.
	undetected := modernBuild
	undetected.Detected = false
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{undetected, cudaFork}}
	got, ok := avail.bestLlamaBinary(BinaryRequirements{})
	if !ok || got.Path != cudaFork.Path {
		t.Fatalf("expected to skip not-detected and pick %q, got %q (ok=%v)", cudaFork.Path, got.Path, ok)
	}
}

func TestBestLlamaBinary_NoneAvailable(t *testing.T) {
	if _, ok := (EngineAvailability{}).bestLlamaBinary(BinaryRequirements{}); ok {
		t.Error("empty availability must return (zero,false)")
	}
}

func TestRequirementsForModel(t *testing.T) {
	tq3 := ModelDescriptor{ID: "t", Format: FormatGGUF, Quantization: "tq3_4s", LocalPath: "x-TQ3_4S.gguf"}
	if r := requirementsForModel(tq3); !r.TQ3 {
		t.Error("TQ3_* model must impose the TQ3 requirement")
	}
	plain := ModelDescriptor{ID: "q", Format: FormatGGUF, Quantization: "iq4_xs", LocalPath: "Qwen3.6-27B-IQ4_XS.gguf"}
	if r := requirementsForModel(plain); r.TQ3 {
		t.Error("non-TQ3 model must impose NO requirement (selected by configured order)")
	}
}

// TestPlanLlamaLaunch_SelectsFirstConfiguredNonTQ3 ties the selection change to the real launch
// path: a non-TQ3 model over [b10106, fork] selects b10106 (first), NOT the has_cuda fork. This
// is the end-to-end M1.GATE proof that b10106 wins without deleting the other binaries.
func TestPlanLlamaLaunch_SelectsFirstConfiguredNonTQ3(t *testing.T) {
	d := ModelDescriptor{
		ID: "qwen3.6-27b-mtp", DisplayName: "Qwen3.6-27B MTP", Format: FormatGGUF, Source: "local_path",
		Quantization: "iq4_xs", LocalPath: "Qwen3.6-27B-IQ4_XS.gguf", GGUFSource: gguf.LocalFile("x.gguf"),
	}
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{modernBuildNoCuda, cudaFork, winget}}
	plan, err := PlanLlamaLaunch(d, avail, LaunchProfile{Ctx: 8192})
	if err != nil {
		t.Fatalf("PlanLlamaLaunch: %v", err)
	}
	if plan.Binary.Path != "b10106" {
		t.Errorf("expected b10106 (first configured, non-TQ3), got %q — has_cuda must not bias selection", plan.Binary.Path)
	}
	// The plan reason should name the selected binary.
	if !strings.Contains(plan.Reason, "b10106") {
		t.Errorf("plan reason should name the selected binary; got %q", plan.Reason)
	}
}

// TestEvaluateGGUF_NonTQ3_SelectsFirstConfigured ties selection to the verdict path: a non-TQ3
// GGUF that fits, evaluated with [b10106 (has_cuda=false), fork], selects b10106 for the engine
// choice detail — proving the compat VERDICT (not just PlanLlamaLaunch) respects config order.
func TestEvaluateGGUF_NonTQ3_SelectsFirstConfigured(t *testing.T) {
	e := fakeGGUFEngine(constGGUF(6000, gguf.ConfidenceApproximate))
	d := ModelDescriptor{ID: "q", DisplayName: "Q", Format: FormatGGUF, Source: "local_path",
		Quantization: "iq4_xs", GGUFSource: gguf.LocalFile("q.gguf")}
	avail := EngineAvailability{LlamaBinaries: []LlamaBinary{modernBuildNoCuda, cudaFork}}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), avail)
	if v.Status != StatusRuns {
		t.Fatalf("status=%q want runs (reason: %s)", v.Status, v.Reason)
	}
	if len(v.EngineChoices) == 0 {
		t.Fatal("expected at least one engine choice")
	}
	got := v.EngineChoices[0]
	// The engine-choice detail comes from the SELECTED binary's Detail. With b10106 configured
	// first and no TQ3 requirement, it must be b10106 — NOT the CUDA fork.
	if got.Detail != "b10106" {
		t.Errorf("non-TQ3 model must select b10106 by config order; engine-choice detail = %q", got.Detail)
	}
}
