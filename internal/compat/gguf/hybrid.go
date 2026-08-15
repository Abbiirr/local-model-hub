package gguf

import (
	"fmt"

	ggufparser "github.com/gpustack/gguf-parser-go"
)

// hybrid.go corrects the KV-cache estimate for HYBRID SSM/attention models (the Qwen3.5/3.6
// "qwen35" architecture, Mamba-style), where only a FRACTION of the blocks hold a KV cache that
// scales with context.
//
// THE DEFECT THIS FIXES (measured on the reference box, 2026-07-25):
// gguf-parser-go prices the KV cache as if EVERY block were a full-attention block. On
// Qwen3.6-27B-MTP-mini that is 65 blocks — but the file actually has only **17** blocks with
// attention K/Q tensors and **48** SSM blocks whose recurrent state is CONSTANT in context length.
// The KV term is therefore inflated by roughly 65/17 ≈ 3.8×:
//
//	requested 8K : reported 14420 MiB total. Weights are ~13036 MiB, so ~1384 MiB was charged for
//	               KV+overhead. 65 blocks × 8192 tok × ~1658 B/tok ≈ 883 MiB + ~500 MiB overhead
//	               = 1383 MiB — matching the reported number almost exactly, which confirms all 65
//	               blocks were priced. The TRUE KV is 17 blocks ≈ 231 MiB.
//	requested 64K: the phantom KV grows to ~5 GiB, which made LocalHub refuse `wont_run` for a
//	               profile the owner has actually RUN on this card with headroom to spare.
//
// A false `wont_run` is a real defect: it makes an honest tool useless for the model in front of
// you. This is the mirror image of iswa.go — there gguf-parser UNDER-priced gemma-4's KV (to ~0)
// and we add a floor; here it OVER-prices and we replace the term with one derived from the
// model's own metadata.
//
// SAFETY (CLAUDE.md: a false `runs` that OOMs is a correctness bug):
//   - Fires ONLY when the architecture is unambiguous: SSM metadata present AND an attention-block
//     count we can actually derive.
//   - Only ever reduces the KV term the library over-counted; it never touches the weights term,
//     and it never fires when our own reconstruction is >= the library's number.
//   - Every term rounds UP: quantized bytes-per-element round up, the SSM state is priced at f32,
//     a block holding BOTH attention and SSM tensors counts as attention, and an interval-derived
//     block count rounds up. Ties fall on the conservative side.
type hybridSSMInfo struct {
	blockCount    int
	attnBlocks    int    // blocks holding a context-scaling KV cache
	ssmBlocks     int    // blocks holding a CONSTANT recurrent state
	kvBytesPerTok uint64 // KV bytes per token across ALL attention blocks, at the requested dtypes
	ssmStateBytes uint64 // total constant SSM state across all SSM blocks
	desc          string
}

// hybridSSMCorrection returns the SIGNED byte delta to apply to the library's estimate plus the
// corrected context-VRAM figure in MiB. ok=false leaves the estimate untouched.
//
// The delta is negative when the library over-counted; the function returns ok=false rather than a
// positive delta, so it can never inflate an estimate (that direction belongs to iswa.go).
func hybridSSMCorrection(cheap *ggufparser.GGUFFile, libKVBytes uint64, opts Options) (deltaBytes int64, ctxVRAMMiB int, note string, ok bool) {
	if cheap == nil || libKVBytes == 0 {
		return 0, 0, "", false
	}
	a := cheap.Architecture()

	effCtx := opts.ContextLen
	if effCtx <= 0 {
		effCtx = int(a.MaximumContextLength)
	}
	if effCtx <= 0 {
		return 0, 0, "", false
	}

	// NO re-parse here, deliberately. The cheap read already carries TensorInfos (SkipLargeMetadata
	// only drops the big tokenizer/vocab METADATA arrays), so countHybridBlocks has everything it needs.
	// An extra ParseGGUFFile would run once per estimate — and evaluateGGUF estimates 4× per verdict
	// (requested ctx + 3 candidates), which blew the <2s blocked-admission budget that
	// TestRealFile_EvaluateUnder2s guards. hybridSSMLayout also bails on the SSM metadata check before
	// touching tensors, so a non-hybrid model costs only a few map lookups.
	kType, vType := resolveKVPair(opts)
	info, iok := hybridSSMLayout(cheap, kType, vType)
	if !iok {
		return 0, 0, "", false
	}

	par := uint64(parallel(opts.ParallelSeq))
	trueKV := info.kvBytesPerTok * uint64(effCtx) * par
	trueSSM := info.ssmStateBytes * par
	corrected := trueKV + trueSSM

	if corrected >= libKVBytes {
		// The library did not over-count (or our reconstruction is larger). Never inflate here.
		return 0, 0, "", false
	}

	note = fmt.Sprintf(
		"hybrid SSM correction: %s. gguf-parser priced a KV cache across all %d blocks (%d MiB); only "+
			"%d blocks hold a context-scaling KV cache, and the %d SSM blocks hold a CONSTANT recurrent "+
			"state. Corrected at ctx=%d, parallel=%d, kv=%s: %d MiB KV + %d MiB constant SSM state "+
			"(reduction of %d MiB)",
		info.desc, info.blockCount, bytesToMiB(libKVBytes), info.attnBlocks, info.ssmBlocks,
		effCtx, par, kvLabelPair(kType, vType),
		bytesToMiB(trueKV), bytesToMiB(trueSSM), bytesToMiB(libKVBytes-corrected))

	return int64(corrected) - int64(libKVBytes), bytesToMiB(corrected), note, true
}

// hybridSSMLayout derives the attention/SSM block split and per-token KV cost from metadata. It
// returns ok=false unless the model is unambiguously hybrid AND every number it needs is present —
// an architecture we cannot fully characterise is left to the library's estimate.
func hybridSSMLayout(f *ggufparser.GGUFFile, kType, vType KVCacheType) (hybridSSMInfo, bool) {
	if f == nil {
		return hybridSSMInfo{}, false
	}
	a := f.Architecture()
	arch := a.Architecture
	if arch == "" {
		return hybridSSMInfo{}, false
	}
	kv, _ := f.Header.MetadataKV.Index([]string{
		arch + ".ssm.conv_kernel",
		arch + ".ssm.state_size",
		arch + ".ssm.inner_size",
		arch + ".full_attention_interval",
		arch + ".attention.key_length",
		arch + ".attention.value_length",
	})
	convKernel, hasConv := u64Of(kv, arch+".ssm.conv_kernel")
	stateSize, hasState := u64Of(kv, arch+".ssm.state_size")
	innerSize, hasInner := u64Of(kv, arch+".ssm.inner_size")
	if !hasConv || !hasState || !hasInner {
		return hybridSSMInfo{}, false // not an SSM model, or not characterisable
	}

	blockCount := int(a.BlockCount)
	if blockCount <= 0 {
		return hybridSSMInfo{}, false
	}

	// COUNT attention blocks from tensors — authoritative, and immune to any off-by-one in the
	// interval convention. Fall back to the interval only when tensor names are unavailable.
	attnBlocks, ssmBlocks := countHybridBlocks(f)
	src := "counted from tensors"
	if attnBlocks == 0 || ssmBlocks == 0 {
		interval, hasInterval := u64Of(kv, arch+".full_attention_interval")
		if !hasInterval || interval == 0 {
			return hybridSSMInfo{}, false
		}
		// Round UP: over-counting attention blocks over-estimates KV (the safe direction).
		attnBlocks = (blockCount + int(interval) - 1) / int(interval)
		ssmBlocks = blockCount - attnBlocks
		src = fmt.Sprintf("derived from %s.full_attention_interval=%d", arch, interval)
	}
	if attnBlocks <= 0 || attnBlocks > blockCount || ssmBlocks < 0 {
		return hybridSSMInfo{}, false
	}

	// Per-token KV across all attention blocks. key_length/value_length are per-head dims.
	keyLen, hasK := u64Of(kv, arch+".attention.key_length")
	valLen, hasV := u64Of(kv, arch+".attention.value_length")
	nKVHeads := uint64(a.AttentionHeadCountKV)
	if !hasK || !hasV || nKVHeads == 0 {
		return hybridSSMInfo{}, false
	}
	// bytes-per-element ×8 (integer math; quantized types round UP — see kvBytesPerElemX8).
	kPerTok := (nKVHeads * keyLen * kvBytesPerElemX8(kType)) / 8
	vPerTok := (nKVHeads * valLen * kvBytesPerElemX8(vType)) / 8
	kvBytesPerTok := (kPerTok + vPerTok) * uint64(attnBlocks)

	// Constant recurrent state per SSM block, priced at f32 (4 B) — what llama.cpp uses for Mamba
	// conv/ssm state, and the conservative choice. conv state: conv_kernel × inner_size;
	// ssm state: state_size × inner_size (group_count only subdivides it, so ignoring the
	// subdivision keeps this an upper bound).
	const ssmStateElemBytes = 4
	perSSMBlock := (convKernel*innerSize + stateSize*innerSize) * ssmStateElemBytes
	ssmStateBytes := perSSMBlock * uint64(ssmBlocks)

	return hybridSSMInfo{
		blockCount:    blockCount,
		attnBlocks:    attnBlocks,
		ssmBlocks:     ssmBlocks,
		kvBytesPerTok: kvBytesPerTok,
		ssmStateBytes: ssmStateBytes,
		desc: fmt.Sprintf("%s hybrid, %d attention + %d SSM of %d blocks (%s)",
			arch, attnBlocks, ssmBlocks, blockCount, src),
	}, true
}

// countHybridBlocks counts blocks carrying attention K/Q tensors versus SSM tensors. A block with
// BOTH counts as an attention block (it has a KV cache) — the conservative reading.
func countHybridBlocks(f *ggufparser.GGUFFile) (attn, ssm int) {
	attnSet, ssmSet := map[int]bool{}, map[int]bool{}
	for _, ti := range f.TensorInfos {
		var blk int
		if _, err := fmt.Sscanf(ti.Name, "blk.%d.", &blk); err != nil {
			continue
		}
		switch {
		case containsFold(ti.Name, "attn_k."), containsFold(ti.Name, "attn_q."):
			attnSet[blk] = true
		case containsFold(ti.Name, "ssm_"):
			ssmSet[blk] = true
		}
	}
	for b := range attnSet {
		delete(ssmSet, b) // a block with attention has a KV cache regardless of what else it holds
	}
	return len(attnSet), len(ssmSet)
}

// libraryKVBytes sums the KV cache the LIBRARY charged across the estimated devices — the number
// the hybrid correction replaces. Same accessor the iSWA path uses, so both corrections reason
// about the identical quantity.
func libraryKVBytes(run ggufparser.LLaMACppRunEstimate) uint64 {
	var b uint64
	for _, d := range run.Devices {
		b += uint64(d.KVCache.Key) + uint64(d.KVCache.Value)
	}
	return b
}

// u64Of reads a numeric metadata value by key from an indexed lookup.
func u64Of(kv map[string]ggufparser.GGUFMetadataKV, key string) (uint64, bool) {
	v, ok := kv[key]
	if !ok {
		return 0, false
	}
	return ggufparser.ValueNumeric[uint64](v), true
}

// containsFold is a small case-insensitive substring check (tensor names are lowercase in practice;
// this guards against a producer that capitalises).
func containsFold(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			c, d := s[i+j], sub[j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if d >= 'A' && d <= 'Z' {
				d += 'a' - 'A'
			}
			if c != d {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
