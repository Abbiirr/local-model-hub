package compat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat/gguf"
	"github.com/localhub/localhub/internal/compat/safetensors"
	"github.com/localhub/localhub/internal/hardware"
)

// --- Test fakes ---------------------------------------------------------------------------

// fakeEngine builds an Engine whose GGUF estimate is driven by a function of the requested
// context, so ctx-sensitivity (max-context search) is testable without a real GGUF.
func fakeGGUFEngine(vramByCtx func(ctx int) (gguf.Result, error)) *Engine {
	e := New()
	e.ggufEstimate = func(_ context.Context, _ gguf.Source, opts gguf.Options) (gguf.Result, error) {
		ctxLen := opts.ContextLen
		if ctxLen == 0 {
			ctxLen = 1 << 20 // "max" sentinel
		}
		return vramByCtx(ctxLen)
	}
	return e
}

// snap builds a hardware snapshot with the given free/total VRAM (MiB) and compute capability.
func snap(freeMiB, totalMiB int, cc string) hardware.Snapshot {
	return hardware.Snapshot{
		GPUs: []hardware.GPU{{
			Index: 0, Name: "Fake 4060 Ti",
			VRAMTotalMiB: totalMiB, VRAMFreeMiB: freeMiB, VRAMUsedMiB: totalMiB - freeMiB,
			ComputeCapability: cc,
		}},
	}
}

// availWith builds engine availability with one llama binary of the given capabilities.
func availWith(bins ...LlamaBinary) EngineAvailability {
	return EngineAvailability{LlamaBinaries: bins}
}

var (
	cudaFork = LlamaBinary{Path: "fork", Detail: "CUDA fork (has_tq3)", Detected: true, HasTQ3: true, HasFlashAttn: true, HasCUDA: true, SupportsKVQuant: true}
	winget   = LlamaBinary{Path: "llama-server", Detail: "winget (Vulkan/CPU)", Detected: true, HasTQ3: false, HasFlashAttn: true, HasCUDA: false, SupportsKVQuant: true}
)

// constGGUF returns a Result with a fixed VRAM at all contexts.
func constGGUF(vram int, conf gguf.Confidence) func(int) (gguf.Result, error) {
	return func(int) (gguf.Result, error) {
		return gguf.Result{VRAMMiB: vram, Confidence: conf, ParserVersion: gguf.ParserVersion, Architecture: "llama"}, nil
	}
}

// --- Schema-level: Reason is ALWAYS non-empty --------------------------------------------

func TestNewVerdictPanicsOnEmptyReason(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("newVerdict with empty reason must panic (schema invariant)")
		}
	}()
	_ = newVerdict("m", StatusRuns, "")
}

func TestNewAlternativePanicsOnEmptyReason(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("newAlternative with empty reason must panic")
		}
	}()
	_ = newAlternative("llamacpp", "", StatusRuns, "")
}

// TestEveryVerdictPathHasReason exercises every branch and asserts the reason is non-empty
// AND the invariants (runs/runs_degraded carry non-empty RunFlags).
func TestEveryVerdictPathHasReason(t *testing.T) {
	ggufDesc := ModelDescriptor{ID: "m", DisplayName: "M", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("x.gguf")}
	stCfg := safetensors.ModelConfig{HiddenSize: 4096, NumHiddenLayers: 32, NumAttentionHeads: 32, MaxPositionEmbeds: 131072}
	stDesc := ModelDescriptor{ID: "s", DisplayName: "S", Format: FormatSafetensors, STConfigResolved: true, STConfig: stCfg, STWeights: safetensors.WeightsInfo{TotalBytes: 16 * 1024 * 1024 * 1024}}

	cases := []struct {
		name  string
		e     *Engine
		req   Request
		snap  hardware.Snapshot
		avail EngineAvailability
	}{
		{"gguf runs", fakeGGUFEngine(constGGUF(6000, gguf.ConfidenceApproximate)), Request{Descriptor: ggufDesc, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(cudaFork)},
		{"gguf wont", fakeGGUFEngine(constGGUF(30000, gguf.ConfidenceApproximate)), Request{Descriptor: ggufDesc, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(cudaFork)},
		{"st runs", New(), Request{Descriptor: stDesc, RequestedCtx: 8192}, snap(24000, 24576, "8.9"), EngineAvailability{VLLMMode: "attach", VLLMAvailable: true}},
		{"st wont cc", New(), Request{Descriptor: stDesc, RequestedCtx: 8192}, snap(24000, 24576, "7.0"), EngineAvailability{VLLMMode: "attach", VLLMAvailable: true}},
		{"no gpu", New(), Request{Descriptor: ggufDesc}, hardware.Snapshot{}, EngineAvailability{}},
		{"bad format", New(), Request{Descriptor: ModelDescriptor{ID: "z", Format: Format("weird")}}, snap(13000, 16380, "8.9"), EngineAvailability{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.e.Evaluate(context.Background(), tc.req, tc.snap, tc.avail)
			if strings.TrimSpace(v.Reason) == "" {
				t.Fatalf("verdict has empty Reason (schema invariant): %+v", v)
			}
			if v.Status == StatusRuns || v.Status == StatusRunsDegraded {
				if len(v.RunFlags) == 0 {
					t.Errorf("%s verdict must carry non-empty RunFlags, got none: %+v", v.Status, v)
				}
			}
			for _, ch := range v.EngineChoices {
				if strings.TrimSpace(ch.Reason) == "" {
					t.Errorf("engine choice %q has empty reason", ch.Engine)
				}
			}
		})
	}
}

// --- Fit / degraded / wont-fit boundaries ------------------------------------------------

func TestGGUF_FitsWithHeadroom_Runs(t *testing.T) {
	// 6000 MiB required vs 13000*0.92 = 11960 usable → runs, comfortably.
	e := fakeGGUFEngine(constGGUF(6000, gguf.ConfidenceApproximate))
	d := ModelDescriptor{ID: "q", DisplayName: "Q", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("q.gguf")}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(cudaFork))
	if v.Status != StatusRuns {
		t.Fatalf("status = %q, want runs (reason: %s)", v.Status, v.Reason)
	}
	if v.RecommendedEngine != "llamacpp" {
		t.Errorf("recommended engine = %q, want llamacpp", v.RecommendedEngine)
	}
	// Reason must name the numbers.
	for _, want := range []string{"6000", "usable", "safety"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason missing %q: %s", want, v.Reason)
		}
	}
	if len(v.RunFlags) == 0 {
		t.Error("runs verdict must have run flags")
	}
}

func TestGGUF_WontFit_NamesTheNumbers(t *testing.T) {
	// 30000 MiB required vs ~11960 usable → wont_run, and no fitting reduced ctx.
	e := fakeGGUFEngine(constGGUF(30000, gguf.ConfidenceApproximate))
	d := ModelDescriptor{ID: "big", DisplayName: "Big", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("big.gguf")}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(cudaFork))
	if v.Status != StatusWontRun {
		t.Fatalf("status = %q, want wont_run (reason: %s)", v.Status, v.Reason)
	}
	// The task's example reason format: names required, usable, total × safety − in use.
	for _, want := range []string{"30000", "usable", "16380", "0.92"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason missing %q: %s", want, v.Reason)
		}
	}
	if len(v.RunFlags) != 0 {
		t.Errorf("wont_run must have NO run flags, got %v", v.RunFlags)
	}
}

func TestGGUF_FitsOnlyAtReducedCtx_Degraded(t *testing.T) {
	// Requested 32K doesn't fit but 8K does → runs_degraded at the reduced ctx.
	byCtx := func(ctx int) (gguf.Result, error) {
		// linear-ish: bigger ctx costs more.
		vram := 5000 + ctx/8 // 8K→6024, 16K→7048, 32K→9096; all under 11960 actually...
		if ctx >= 1<<20 {    // "max" sentinel → huge
			vram = 40000
		}
		if ctx == 32768 {
			vram = 20000 // force 32K to not fit
		}
		return gguf.Result{VRAMMiB: vram, Confidence: gguf.ConfidenceApproximate, ParserVersion: gguf.ParserVersion}, nil
	}
	e := fakeGGUFEngine(byCtx)
	d := ModelDescriptor{ID: "r", DisplayName: "R", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("r.gguf")}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 32768}, snap(13000, 16380, "8.9"), availWith(cudaFork))
	if v.Status != StatusRunsDegraded {
		t.Fatalf("status = %q, want runs_degraded (reason: %s)", v.Status, v.Reason)
	}
	if v.MaxContextThatFits == 0 || v.MaxContextThatFits >= 32768 {
		t.Errorf("max context that fits = %d, want a positive value < 32768", v.MaxContextThatFits)
	}
	if len(v.RunFlags) == 0 {
		t.Error("degraded verdict must carry run flags at the reduced ctx")
	}
}

// --- TQ3 fork requirement rule -----------------------------------------------------------

func TestTQ3_RequiresFork_WontRunWithoutIt(t *testing.T) {
	e := fakeGGUFEngine(constGGUF(14000, gguf.ConfidenceFallback))
	d := ModelDescriptor{ID: "tq3", DisplayName: "Qwen3.6-27B-TQ3_4S", Format: FormatGGUF, Source: "local_path",
		Quantization: "tq3_4s", LocalPath: "Qwen3.6-27B-TQ3_4S.gguf", GGUFSource: gguf.LocalFile("x.gguf")}
	// Only the winget (no tq3) binary available.
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(winget))
	if v.Status != StatusWontRun {
		t.Fatalf("status = %q, want wont_run (TQ3 without fork). reason: %s", v.Status, v.Reason)
	}
	if !strings.Contains(strings.ToLower(v.Reason), "tq3") || !strings.Contains(strings.ToLower(v.Reason), "fork") {
		t.Errorf("reason must name TQ3 + the fork: %s", v.Reason)
	}
}

func TestTQ3_WithFork_Runs(t *testing.T) {
	// Fallback confidence, comfortably under boundary → runs (or degraded if near boundary).
	e := fakeGGUFEngine(constGGUF(9000, gguf.ConfidenceFallback))
	d := ModelDescriptor{ID: "tq3", DisplayName: "Qwen3.6-27B-TQ3_4S", Format: FormatGGUF, Source: "local_path",
		Quantization: "tq3_4s", LocalPath: "Qwen3.6-27B-TQ3_4S.gguf", GGUFSource: gguf.LocalFile("x.gguf")}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(cudaFork, winget))
	if v.Status != StatusRuns && v.Status != StatusRunsDegraded {
		t.Fatalf("status = %q, want runs/runs_degraded via CUDA fork. reason: %s", v.Status, v.Reason)
	}
	if v.Confidence != ConfidenceFallback {
		t.Errorf("confidence = %q, want fallback (TQ3 estimate is a heuristic)", v.Confidence)
	}
	if v.EngineChoices[0].Detail == "" || !strings.Contains(v.EngineChoices[0].Detail, "CUDA") {
		t.Errorf("engine choice should name the CUDA fork, got detail %q", v.EngineChoices[0].Detail)
	}
}

func TestTQ3_FallbackWontRun_ReasonNamesFallback(t *testing.T) {
	// A TQ3 fallback estimate well over the boundary → wont_run, but the reason MUST name the
	// fallback and the tq3_0 KV blind spot (the real-box Qwen3.6-27B-TQ3_4S case).
	e := fakeGGUFEngine(constGGUF(16178, gguf.ConfidenceFallback))
	d := ModelDescriptor{ID: "tq3", DisplayName: "Qwen3.6-27B-TQ3_4S", Format: FormatGGUF, Source: "local_path",
		Quantization: "tq3_4s", GGUFSource: gguf.LocalFile("x.gguf")}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13289, 16380, "8.9"), availWith(cudaFork, winget))
	if v.Status != StatusWontRun {
		t.Fatalf("status = %q, want wont_run. reason: %s", v.Status, v.Reason)
	}
	low := strings.ToLower(v.Reason)
	if !strings.Contains(low, "fallback") {
		t.Errorf("wont_run reason for a fallback estimate MUST name 'fallback': %s", v.Reason)
	}
	if !strings.Contains(low, "tq3_0") {
		t.Errorf("reason should name the tq3_0 KV blind spot: %s", v.Reason)
	}
}

func TestTQ3_FallbackNearBoundary_Degraded(t *testing.T) {
	// 11500 required vs 11960 usable → within ±15% AND fallback → runs_degraded.
	e := fakeGGUFEngine(constGGUF(11500, gguf.ConfidenceFallback))
	d := ModelDescriptor{ID: "tq3", DisplayName: "TQ3", Format: FormatGGUF, Source: "local_path",
		Quantization: "tq3_4s", GGUFSource: gguf.LocalFile("x.gguf")}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(cudaFork))
	if v.Status != StatusRunsDegraded {
		t.Fatalf("status = %q, want runs_degraded (fallback near boundary). reason: %s", v.Status, v.Reason)
	}
	if !strings.Contains(strings.ToLower(v.Reason), "fallback") {
		t.Errorf("reason must explain the fallback near-boundary caution: %s", v.Reason)
	}
}

// --- safetensors → vLLM only + CC gate ---------------------------------------------------

func TestSafetensors_RoutesToVLLM_NeverGGUF(t *testing.T) {
	cfg := safetensors.ModelConfig{HiddenSize: 4096, NumHiddenLayers: 32, NumAttentionHeads: 32,
		NumKeyValueHeads: intp(8), MaxPositionEmbeds: 131072, TorchDtype: "bfloat16"}
	d := ModelDescriptor{ID: "l31", DisplayName: "Llama-3.1-8B", Format: FormatSafetensors,
		STConfigResolved: true, STConfig: cfg, STWeights: safetensors.WeightsInfo{TotalBytes: 16 * 1024 * 1024 * 1024, Source: safetensors.WeightsFromIndex}}
	v := New().Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(24000, 24576, "8.9"),
		EngineAvailability{VLLMMode: "attach", VLLMAvailable: true})
	if v.RecommendedEngine != "vllm" {
		t.Fatalf("recommended engine = %q, want vllm (safetensors routes to vLLM only)", v.RecommendedEngine)
	}
	for _, ch := range v.EngineChoices {
		if ch.Engine == "llamacpp" {
			t.Error("safetensors must NEVER produce a llama.cpp/GGUF choice (ADR-004)")
		}
	}
	if v.Estimator != "localhub-safetensors" {
		t.Errorf("estimator = %q, want localhub-safetensors", v.Estimator)
	}
}

func TestSafetensors_CCGate_BelowThreshold_WontRun(t *testing.T) {
	cfg := safetensors.ModelConfig{HiddenSize: 4096, NumHiddenLayers: 32, NumAttentionHeads: 32, NumKeyValueHeads: intp(8), MaxPositionEmbeds: 131072}
	d := ModelDescriptor{ID: "s", DisplayName: "S", Format: FormatSafetensors, STConfigResolved: true, STConfig: cfg,
		STWeights: safetensors.WeightsInfo{TotalBytes: 8 * 1024 * 1024 * 1024}}
	// CC 7.0 < 7.5 → wont_run regardless of VRAM.
	v := New().Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(40000, 48000, "7.0"),
		EngineAvailability{VLLMMode: "attach", VLLMAvailable: true})
	if v.Status != StatusWontRun {
		t.Fatalf("status = %q, want wont_run (CC gate). reason: %s", v.Status, v.Reason)
	}
	if !strings.Contains(v.Reason, "7.5") || !strings.Contains(v.Reason, "7.0") {
		t.Errorf("reason must name the CC requirement and actual: %s", v.Reason)
	}
}

func TestSafetensors_CCGate_ReferenceBoxPasses(t *testing.T) {
	cfg := safetensors.ModelConfig{HiddenSize: 4096, NumHiddenLayers: 32, NumAttentionHeads: 32, NumKeyValueHeads: intp(8), MaxPositionEmbeds: 131072}
	d := ModelDescriptor{ID: "s", DisplayName: "S", Format: FormatSafetensors, STConfigResolved: true, STConfig: cfg,
		STWeights: safetensors.WeightsInfo{TotalBytes: 8 * 1024 * 1024 * 1024}}
	// CC 8.9 passes the gate; small model fits on a big card.
	v := New().Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(40000, 48000, "8.9"),
		EngineAvailability{VLLMMode: "attach", VLLMAvailable: true})
	if v.Status == StatusWontRun && strings.Contains(v.Reason, "compute capability") {
		t.Fatalf("CC 8.9 must PASS the gate, got wont_run on CC: %s", v.Reason)
	}
}

func TestSafetensors_DockerMode_Degraded(t *testing.T) {
	cfg := safetensors.ModelConfig{HiddenSize: 4096, NumHiddenLayers: 32, NumAttentionHeads: 32, NumKeyValueHeads: intp(8), MaxPositionEmbeds: 131072}
	d := ModelDescriptor{ID: "s", DisplayName: "S", Format: FormatSafetensors, STConfigResolved: true, STConfig: cfg,
		STWeights: safetensors.WeightsInfo{TotalBytes: 8 * 1024 * 1024 * 1024}}
	v := New().Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(40000, 48000, "8.9"),
		EngineAvailability{VLLMMode: "docker", VLLMAvailable: true})
	if v.Status != StatusRunsDegraded {
		t.Fatalf("status = %q, want runs_degraded (docker cold start). reason: %s", v.Status, v.Reason)
	}
	if !strings.Contains(strings.ToLower(v.Reason), "docker") || !strings.Contains(strings.ToLower(v.Reason), "cold start") {
		t.Errorf("reason must mention docker cold start: %s", v.Reason)
	}
}

// --- Estimator error → unknown -----------------------------------------------------------

func TestGGUF_EstimatorError_Unknown(t *testing.T) {
	e := New()
	e.ggufEstimate = func(context.Context, gguf.Source, gguf.Options) (gguf.Result, error) {
		return gguf.Result{}, errors.New("remote unparseable: no local bytes")
	}
	d := ModelDescriptor{ID: "u", DisplayName: "U", Format: FormatGGUF, Source: "huggingface", Repo: "x/y", File: "z.gguf", GGUFSource: gguf.HFRepo("x/y", "z.gguf", "")}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(cudaFork))
	if v.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown (estimator errored). reason: %s", v.Status, v.Reason)
	}
	// Reason must say what to do.
	if !strings.Contains(strings.ToLower(v.Reason), "download") && !strings.Contains(strings.ToLower(v.Reason), "local") {
		t.Errorf("unknown reason must tell the user what to do: %s", v.Reason)
	}
}

// --- MaxContext cap ----------------------------------------------------------------------

func TestMaxContext_CappedByCandidates(t *testing.T) {
	// Everything fits (tiny model) → max fitting candidate is 32768 (the largest probed).
	e := fakeGGUFEngine(constGGUF(2000, gguf.ConfidenceApproximate))
	d := ModelDescriptor{ID: "tiny", DisplayName: "Tiny", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("t.gguf")}
	v := e.Evaluate(context.Background(), Request{Descriptor: d, RequestedCtx: 8192}, snap(13000, 16380, "8.9"), availWith(cudaFork))
	if v.MaxContextThatFits != 32768 {
		t.Errorf("max context = %d, want 32768 (largest candidate, all fit)", v.MaxContextThatFits)
	}
}

// --- SafetyFactor consistency with the safetensors package -------------------------------

func TestSafetyFactorMatchesSafetensors(t *testing.T) {
	// The safetensors package applies 0.92 internally via FitsInVRAM. Cross-check the compat
	// constant against a boundary: a model needing exactly free*0.92 fits, one needing more does not.
	cfg := safetensors.ModelConfig{HiddenSize: 4096, NumHiddenLayers: 32, NumAttentionHeads: 32, NumKeyValueHeads: intp(8), MaxPositionEmbeds: 131072}
	est, err := safetensors.EstimateVRAM(cfg, safetensors.WeightsInfo{TotalBytes: int64(9000) * 1024 * 1024}, safetensors.Params{Ctx: 1, Batch: 1})
	if err != nil {
		t.Fatal(err)
	}
	free := est.TotalMiB / SafetyFactor // free s.t. usable == required exactly
	if !est.FitsInVRAM(free + 1) {
		t.Error("model should fit when usable slightly exceeds required")
	}
	if est.FitsInVRAM(free - 1) {
		t.Error("model should NOT fit when usable slightly below required (safety_factor mismatch)")
	}
}

func intp(i int) *int { return &i }
