package gguf

import (
	"context"
	"strconv"
	"strings"

	ggufparser "github.com/gpustack/gguf-parser-go"
)

// MoEInfo describes a GGUF model's mixture-of-experts structure, enough to price a
// CPU expert-offload (llama.cpp `-ncmoe N` / `--n-cpu-moe N`, verified in the local
// b8833 --help: "keep MoE weights of first N layers on CPU"; research §0/§4).
//
// The honest-sizing rule (CLAUDE.md "no speculative admission"): admission may only
// subtract the expert weight from the GPU footprint when ExpertBytes was actually
// DERIVED from the tensor metadata (Derivable == true). If the expert tensors cannot
// be measured, callers MUST decline the offload rather than guess a smaller footprint.
type MoEInfo struct {
	// IsMoE is true when the model declares experts (<arch>.expert_count > 0).
	IsMoE bool
	// ExpertCount is <arch>.expert_count (experts per MoE layer).
	ExpertCount int
	// ExpertLayers is the number of transformer layers that carry expert tensors —
	// the maximum meaningful N for `-ncmoe N` (offload every expert layer to CPU).
	ExpertLayers int
	// ExpertBytes is the summed on-disk size of the expert (`*_exps*`) tensors: the
	// weight that leaves VRAM when the experts are placed on CPU. 0 when not derivable.
	ExpertBytes uint64
	// Derivable is true when ExpertBytes was summed from real tensor metadata (so an
	// offload-aware footprint is safe to compute). False => callers must decline.
	Derivable bool
}

// ExpertBytesMiB is ExpertBytes rounded down to MiB.
func (m MoEInfo) ExpertBytesMiB() int { return int(m.ExpertBytes / MiB) }

// expertTensorMarker identifies MoE expert weight tensors in a llama.cpp GGUF. The
// stable convention is a `_exps` infix (e.g. `blk.3.ffn_gate_exps.weight`,
// `ffn_down_exps.weight`, `ffn_up_exps.weight`). Bias/norm tensors that share the
// stem are still MoE expert data and correctly counted.
const expertTensorMarker = "_exps"

// InspectMoE reads a GGUF's header + tensor-info section (NO model load, NO GPU) and
// reports its MoE structure. Tensor infos are always parsed (even under
// SkipLargeMetadata), so a local inspect is as cheap as the estimate parse. A parse
// failure is returned as (zero, err); a non-MoE model is (IsMoE:false, nil).
func InspectMoE(ctx context.Context, source Source) (MoEInfo, error) {
	f, err := parseSource(ctx, source)
	if err != nil {
		return MoEInfo{}, err
	}
	return moeFromFile(f), nil
}

// moeFromFile computes MoEInfo from an already-parsed GGUF (unit-testable with a
// synthetic file).
func moeFromFile(f *ggufparser.GGUFFile) MoEInfo {
	if f == nil {
		return MoEInfo{}
	}
	a := f.Architecture()
	info := MoEInfo{ExpertCount: int(a.ExpertCount), IsMoE: a.ExpertCount > 0}
	if !info.IsMoE {
		return info
	}
	layers := map[int]struct{}{}
	for _, ti := range f.TensorInfos {
		if !strings.Contains(ti.Name, expertTensorMarker) {
			continue
		}
		info.ExpertBytes += ti.Bytes()
		if li, ok := blockIndex(ti.Name); ok {
			layers[li] = struct{}{}
		}
	}
	info.ExpertLayers = len(layers)
	// Derivable requires BOTH real expert bytes AND at least one identified layer, so a
	// model that declares experts but exposes no measurable expert tensors (e.g. a
	// packed/quirk layout we don't recognise) is declined rather than mis-priced.
	info.Derivable = info.ExpertBytes > 0 && info.ExpertLayers > 0
	return info
}

// blockIndex extracts N from a `blk.N.<...>` tensor name.
func blockIndex(name string) (int, bool) {
	const p = "blk."
	i := strings.Index(name, p)
	if i < 0 {
		return 0, false
	}
	rest := name[i+len(p):]
	j := strings.IndexByte(rest, '.')
	if j <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0, false
	}
	return n, true
}
