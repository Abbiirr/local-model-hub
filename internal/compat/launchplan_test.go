package compat

import (
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// nglValue returns the token following "-ngl" in flags, or "" if absent.
func nglValue(flags []string) string {
	for i, f := range flags {
		if f == "-ngl" && i+1 < len(flags) {
			return flags[i+1]
		}
	}
	return ""
}

// TestPlanLlamaLaunch_NGLDefaultVsCPUOffload is the core regression for the full-GPU directive:
// the default (allowCPUOffload=false) must emit "-ngl all" (never offload to CPU); opting in
// (allowCPUOffload=true) must emit "-ngl auto" (llama.cpp offloads as many layers as fit, rest on
// CPU). Verified against the local llama-server --help (2026-07-07): -ngl accepts an exact number,
// 'auto', or 'all' (default: auto).
func TestPlanLlamaLaunch_NGLDefaultVsCPUOffload(t *testing.T) {
	d := ModelDescriptor{ID: "m", DisplayName: "M", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("x.gguf")}
	avail := availWith(winget)

	// Default: full GPU.
	full, err := PlanLlamaLaunch(d, avail, 8192, false, false, SpeculativeOff)
	if err != nil {
		t.Fatalf("PlanLlamaLaunch full-gpu: %v", err)
	}
	if got := nglValue(full.Flags); got != "all" {
		t.Errorf("default -ngl: got %q want \"all\" (full GPU, never CPU)", got)
	}

	// Opt-in: allow CPU offload.
	split, err := PlanLlamaLaunch(d, avail, 8192, false, true, SpeculativeOff)
	if err != nil {
		t.Fatalf("PlanLlamaLaunch cpu-offload: %v", err)
	}
	if got := nglValue(split.Flags); got != "auto" {
		t.Errorf("allow_cpu_offload -ngl: got %q want \"auto\" (GPU/CPU split)", got)
	}
	if !strings.Contains(split.Reason, "CPU offload allowed") {
		t.Errorf("cpu-offload plan reason should note the split; got %q", split.Reason)
	}
}

// TestGGUFRunFlags_NGL asserts the low-level flag builder directly (both branches), independent of
// binary selection.
func TestGGUFRunFlags_NGL(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}
	bin := winget

	full := ggufRunFlags(d, bin, 4096, gguf.KVCacheF16, false, false, SpeculativeOff)
	if got := nglValue(full); got != "all" {
		t.Errorf("ggufRunFlags default -ngl: got %q want \"all\"", got)
	}
	split := ggufRunFlags(d, bin, 4096, gguf.KVCacheF16, false, true, SpeculativeOff)
	if got := nglValue(split); got != "auto" {
		t.Errorf("ggufRunFlags allowCPUOffload -ngl: got %q want \"auto\"", got)
	}
}

// hasFlagPair reports whether flags contains name immediately followed by val.
func hasFlagPair(flags []string, name, val string) bool {
	for i, f := range flags {
		if f == name && i+1 < len(flags) && flags[i+1] == val {
			return true
		}
	}
	return false
}

// TestGGUFRunFlags_Speculative asserts the draft-free self-speculative opt-in: off (default) emits
// no --spec-type; "ngram-cache" appends exactly `--spec-type ngram-cache` (the value verified from
// the local b8833 --help, research §0). It must never change -ngl (adds ~zero VRAM, flags only).
func TestGGUFRunFlags_Speculative(t *testing.T) {
	d := ModelDescriptor{ID: "m", Format: FormatGGUF}
	bin := winget

	off := ggufRunFlags(d, bin, 4096, gguf.KVCacheF16, false, false, SpeculativeOff)
	for i, f := range off {
		if f == "--spec-type" {
			t.Fatalf("speculative off must not emit --spec-type; flags=%v (at %d)", off, i)
		}
	}
	on := ggufRunFlags(d, bin, 4096, gguf.KVCacheF16, false, false, SpeculativeNgramCache)
	if !hasFlagPair(on, "--spec-type", "ngram-cache") {
		t.Errorf("speculative ngram-cache must emit `--spec-type ngram-cache`; flags=%v", on)
	}
	if got := nglValue(on); got != "all" {
		t.Errorf("speculative must not change -ngl; got %q want \"all\"", got)
	}
}

// TestPlanLlamaLaunch_SpeculativeReason confirms the launch plan threads the speculative value into
// its emitted flags and notes it in the human reason (CLAUDE.md: verdicts carry a reason).
func TestPlanLlamaLaunch_SpeculativeReason(t *testing.T) {
	d := ModelDescriptor{ID: "m", DisplayName: "M", Format: FormatGGUF, Source: "local_path", GGUFSource: gguf.LocalFile("x.gguf")}
	avail := availWith(winget)

	plan, err := PlanLlamaLaunch(d, avail, 8192, false, false, SpeculativeNgramCache)
	if err != nil {
		t.Fatalf("PlanLlamaLaunch speculative: %v", err)
	}
	if !hasFlagPair(plan.Flags, "--spec-type", "ngram-cache") {
		t.Errorf("plan flags should include `--spec-type ngram-cache`; got %v", plan.Flags)
	}
	if !strings.Contains(plan.Reason, "self-speculative") {
		t.Errorf("plan reason should note self-speculative decoding; got %q", plan.Reason)
	}
}

// TestValidateSpeculative covers the allow-list: "" and "ngram-cache" pass; anything else is
// rejected with a reason naming the valid values.
func TestValidateSpeculative(t *testing.T) {
	for _, ok := range []string{SpeculativeOff, SpeculativeNgramCache} {
		if got, err := ValidateSpeculative(ok); err != nil || got != ok {
			t.Errorf("ValidateSpeculative(%q) = (%q,%v), want (%q,nil)", ok, got, err, ok)
		}
	}
	for _, bad := range []string{"ngram", "ngram-simple", "draft-eagle3", "on", "true"} {
		if _, err := ValidateSpeculative(bad); err == nil {
			t.Errorf("ValidateSpeculative(%q) should be rejected", bad)
		}
	}
}
