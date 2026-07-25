package compat

import (
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// hasPair reports whether flags contains name immediately followed by val.
func hasPair(flags []string, name, val string) bool {
	for i, f := range flags {
		if f == name && i+1 < len(flags) && flags[i+1] == val {
			return true
		}
	}
	return false
}

// TestMoEOffloadDecision_Derivable: a MoE model whose expert bytes are derivable emits
// `-ncmoe <expertLayers>`, reports the offloaded MiB, and states the offload in the reason.
func TestMoEOffloadDecision_Derivable(t *testing.T) {
	moe := gguf.MoEInfo{IsMoE: true, ExpertCount: 128, ExpertLayers: 46, ExpertBytes: 6 * 1024 * 1024 * 1024, Derivable: true}
	flags, offloadMiB, reason, err := moeOffloadDecision(moe, "qwen3.6-35b-a3b")
	if err != nil {
		t.Fatalf("derivable MoE should not error: %v", err)
	}
	if !hasPair(flags, "-ncmoe", "46") {
		t.Errorf("want `-ncmoe 46`; got %v", flags)
	}
	if offloadMiB != 6144 {
		t.Errorf("offloadMiB = %d, want 6144 (6 GiB expert weight)", offloadMiB)
	}
	if !strings.Contains(reason, "MoE experts on CPU: 46 layers") || !strings.Contains(reason, "allow_cpu_offload") {
		t.Errorf("reason should state the MoE offload; got %q", reason)
	}
}

// TestMoEOffloadDecision_NotDerivable: a MoE that opts into offload but whose expert
// bytes are NOT derivable must DECLINE with the mandated reason — never a guessed footprint.
func TestMoEOffloadDecision_NotDerivable(t *testing.T) {
	moe := gguf.MoEInfo{IsMoE: true, ExpertCount: 128, ExpertLayers: 0, ExpertBytes: 0, Derivable: false}
	flags, offloadMiB, _, err := moeOffloadDecision(moe, "some-moe")
	if err == nil {
		t.Fatal("non-derivable MoE offload must return an error (decline), not proceed")
	}
	if !strings.Contains(err.Error(), "MoE offload sizing not derivable for this model") {
		t.Errorf("decline reason must match the contract phrasing; got %q", err.Error())
	}
	if len(flags) != 0 || offloadMiB != 0 {
		t.Errorf("a declined offload must emit no flags and no footprint; got flags=%v offload=%d", flags, offloadMiB)
	}
}

// TestMoEOffloadDecision_NonMoE: a non-MoE model is a no-op (no flags, no error) — the
// caller keeps the plain -ngl auto offload.
func TestMoEOffloadDecision_NonMoE(t *testing.T) {
	flags, offloadMiB, reason, err := moeOffloadDecision(gguf.MoEInfo{IsMoE: false}, "dense-model")
	if err != nil || len(flags) != 0 || offloadMiB != 0 || reason != "" {
		t.Errorf("non-MoE must be a clean no-op; got flags=%v offload=%d reason=%q err=%v", flags, offloadMiB, reason, err)
	}
}
