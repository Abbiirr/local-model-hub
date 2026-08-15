package gguf

import (
	"context"
	"strings"

	ggufparser "github.com/gpustack/gguf-parser-go"
)

// MTPInfo describes a GGUF model's embedded multi-token-prediction (MTP) head — the
// "next-n" draft module llama.cpp loads with `--spec-type draft-mtp` (no separate draft
// model; the head is baked into the weights). It gates MTP on the MODEL side (M3.2); the
// BINARY side is gated separately on has_spec_mtp (M3.1/M3.3).
//
// Tensor convention (verified primary source, 2026-07-25, on the actual target file
// Qwen3.6-27B-16GB-VRAM-MTP-mini-IQ4_XS.gguf): the MTP head tensors live under the final
// block with a `.nextn.` infix — `blk.64.nextn.eh_proj.weight` (≈53 MiB) plus
// `nextn.enorm/hnorm/shared_head_norm`. We match the `.nextn.` marker (the Qwen3
// "next-n" naming) and also accept `mtp`/`draft` for forward compatibility.
type MTPInfo struct {
	// IsMTP is true when at least one MTP/nextn tensor was found in the file.
	IsMTP bool
	// HeadBytes is the summed on-disk size of the MTP-head tensors — the extra VRAM the
	// embedded draft head costs when MTP is enabled (M4.2). 0 when not an MTP model.
	HeadBytes uint64
	// TensorCount is the number of MTP-head tensors found (observability/sanity).
	TensorCount int
}

// HeadBytesMiB is HeadBytes rounded down to MiB.
func (m MTPInfo) HeadBytesMiB() int { return int(m.HeadBytes / MiB) }

// mtpTensorMarkers identify the embedded MTP/next-n draft head. `.nextn.` is the verified
// Qwen3 convention; "mtp"/"draft" are accepted for forward compat with other arches.
var mtpTensorMarkers = []string{".nextn.", "mtp.", ".mtp_", "draft."}

// InspectMTP reads a GGUF's header + tensor-info section (NO model load, NO GPU) and reports
// whether the file embeds an MTP draft head and its weight cost. A parse failure is returned
// as (zero, err); a non-MTP model is (IsMTP:false, nil). Cheap: tensor infos are always parsed.
func InspectMTP(ctx context.Context, source Source) (MTPInfo, error) {
	f, err := parseSource(ctx, source)
	if err != nil {
		return MTPInfo{}, err
	}
	return mtpFromFile(f), nil
}

// mtpFromFile computes MTPInfo from an already-parsed GGUF (unit-testable).
func mtpFromFile(f *ggufparser.GGUFFile) MTPInfo {
	info := MTPInfo{}
	if f == nil {
		return info
	}
	for _, ti := range f.TensorInfos {
		if !isMTPTensor(ti.Name) {
			continue
		}
		info.IsMTP = true
		info.HeadBytes += ti.Bytes()
		info.TensorCount++
	}
	return info
}

// isMTPTensor reports whether a tensor name belongs to the embedded MTP/next-n draft head.
func isMTPTensor(name string) bool {
	low := strings.ToLower(name)
	for _, m := range mtpTensorMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
