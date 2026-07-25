package gguf

import (
	"testing"

	ggufparser "github.com/gpustack/gguf-parser-go"
)

// tensorInfo builds a GGUFTensorInfo with a known element count and dtype so Bytes()
// is well-defined. A 1-D tensor of `elems` F16 elements = elems*2 bytes.
func tensorInfoF16(name string, elems uint64) ggufparser.GGUFTensorInfo {
	return ggufparser.GGUFTensorInfo{
		Name: name, NDimensions: 1, Dimensions: []uint64{elems}, Type: ggufparser.GGMLTypeF16,
	}
}

func fakeMoE(expertCount uint32, tis ...ggufparser.GGUFTensorInfo) *ggufparser.GGUFFile {
	kvs := ggufparser.GGUFMetadataKVs{
		strKV("general.architecture", "qwen3moe"),
		scalarU32("qwen3moe.block_count", 4),
		scalarU32("qwen3moe.expert_count", expertCount),
	}
	return &ggufparser.GGUFFile{
		Header:      ggufparser.GGUFHeader{MetadataKVCount: uint64(len(kvs)), MetadataKV: kvs},
		TensorInfos: ggufparser.GGUFTensorInfos(tis),
	}
}

// TestMoEFromFile_DerivesExpertBytes: a MoE model with `_exps` tensors on 2 layers is
// detected, its expert bytes summed, its expert-layer count found, and Derivable=true.
func TestMoEFromFile_DerivesExpertBytes(t *testing.T) {
	f := fakeMoE(8,
		tensorInfoF16("blk.0.ffn_gate_exps.weight", 1000),
		tensorInfoF16("blk.0.ffn_up_exps.weight", 1000),
		tensorInfoF16("blk.1.ffn_down_exps.weight", 2000),
		tensorInfoF16("blk.0.attn_q.weight", 500), // non-expert, must be excluded
	)
	m := moeFromFile(f)
	if !m.IsMoE || m.ExpertCount != 8 {
		t.Fatalf("IsMoE/ExpertCount = %v/%d, want true/8", m.IsMoE, m.ExpertCount)
	}
	if m.ExpertLayers != 2 {
		t.Errorf("ExpertLayers = %d, want 2 (blk.0 + blk.1)", m.ExpertLayers)
	}
	// (1000+1000+2000) F16 elems * 2 B = 8000 B.
	if m.ExpertBytes != 8000 {
		t.Errorf("ExpertBytes = %d, want 8000 (excludes the non-_exps attn tensor)", m.ExpertBytes)
	}
	if !m.Derivable {
		t.Error("Derivable should be true when expert bytes + layers were found")
	}
}

// TestMoEFromFile_NonMoE: a model with no expert_count is not MoE.
func TestMoEFromFile_NonMoE(t *testing.T) {
	kvs := ggufparser.GGUFMetadataKVs{
		strKV("general.architecture", "llama"),
		scalarU32("llama.block_count", 32),
	}
	f := &ggufparser.GGUFFile{Header: ggufparser.GGUFHeader{MetadataKV: kvs}}
	if m := moeFromFile(f); m.IsMoE || m.Derivable {
		t.Errorf("non-MoE model: got IsMoE=%v Derivable=%v, want false/false", m.IsMoE, m.Derivable)
	}
}

// TestMoEFromFile_DeclaredButNoExpertTensors: expert_count>0 but no measurable `_exps`
// tensors → Derivable=false (callers must decline, never guess).
func TestMoEFromFile_DeclaredButNoExpertTensors(t *testing.T) {
	f := fakeMoE(8, tensorInfoF16("blk.0.attn_q.weight", 500)) // no _exps tensors
	m := moeFromFile(f)
	if !m.IsMoE {
		t.Fatal("expert_count>0 must set IsMoE")
	}
	if m.Derivable {
		t.Error("Derivable must be false when no expert tensors are measurable")
	}
}
