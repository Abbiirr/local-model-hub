package gguf

import (
	"context"
	"fmt"

	ggufparser "github.com/gpustack/gguf-parser-go"
)

// iSWA (interleaved sliding-window attention) KV correction.
//
// WHY THIS EXISTS (correctness, CLAUDE.md "no false `runs` that OOMs"):
// gguf-parser-go v0.24.1 does NOT model Gemma-4's interleaved SWA. Two concrete
// mis-behaviours were measured on the real gemma-4-12B GGUF (general.architecture
// = "gemma4", header-only read, 2026-07-08):
//
//  1. The library's architecture table has cases for "gemma2"/"gemma3" but NONE
//     for "gemma4", so AttentionSlidingWindowPattern stays at its default 1
//     ("no SWA") — the estimator would price FULL-context KV on every layer.
//  2. Gemma-4 stores attention.head_count_kv as a PER-LAYER array (type 9), and
//     LocalHub's local parse uses SkipLargeMetadata() (the P6.1 perf fix), under
//     which that array is dropped and AttentionHeadCountKV resolves to 0. With
//     zero KV heads the estimator prices the KV cache at **0 MiB at every ctx**.
//
// Net effect: LocalHub UNDER-prices gemma-4 KV (priced 0). That is the dangerous
// direction — a false "fits" that OOMs at high context. Per ADR-002 we do NOT
// fork the estimator; we WRAP it: compute an iSWA-aware KV floor from the model's
// own metadata and, when the library under-prices, add the shortfall to the VRAM
// estimate. Gemma-4's real KV is heterogeneous and only the metadata describes it:
//   - sliding (local) layers: window-capped KV, head_count_kv≈8, head_dim 256.
//   - global layers (every Nth): full-ctx KV, head_count_kv≈1, head_dim 512.
//
// This floor is intentionally conservative: sliding layers are sized to the window
// PLUS a logical-batch allowance (matching llama.cpp's SWA cache allocation
// `min(nCtx, n_swa + n_batch)`), so the correction never under-states the real
// allocation. Values are read straight from the GGUF, so no engine behaviour is
// invented (CLAUDE.md).

// swaBatchAllowance is the logical-batch headroom llama.cpp adds to the sliding-
// window KV cache (its SWA cache is sized to hold the window plus a batch of new
// tokens: min(nCtx, n_swa*n_seq + n_batch)). llama.cpp's default logical batch is
// 2048; using it keeps the floor conservative (never under the real allocation).
const swaBatchAllowance = 2048

// iswaCorrection decides whether the library under-priced an SWA model's KV cache
// and, if so, returns the number of BYTES to add to the VRAM estimate, the iSWA KV
// floor in MiB (for Result.CtxVRAMMiB), and a human note. It is a no-op (ok=false)
// for non-SWA models or when the library's KV number already meets/exceeds the floor.
//
// LocalHub's local parse uses SkipLargeMetadata(), which drops Gemma-4's per-layer
// head_count_kv / sliding-window-pattern arrays — exactly the metadata the floor
// needs. So for a LOCAL source we do ONE extra full-metadata parse (mmap, no skip)
// and only when the cheap parse already flagged the model as SWA (AttentionSliding
// Window > 0), so the extra read is paid only for the rare SWA model, never on the
// hot path for ordinary models. Remote parses already carry the arrays, so the
// already-parsed file is reused.
func iswaCorrection(ctx context.Context, source Source, cheap *ggufparser.GGUFFile, run ggufparser.LLaMACppRunEstimate, opts Options) (addBytes uint64, floorMiB int, note string, ok bool) {
	if cheap == nil {
		return 0, 0, "", false
	}
	a := cheap.Architecture()
	if a.AttentionSlidingWindow == 0 {
		return 0, 0, "", false // not an SWA model — nothing to correct
	}

	// Effective context: mirror the estimator (0 => the model's trained maximum).
	effCtx := opts.ContextLen
	if effCtx <= 0 {
		effCtx = int(a.MaximumContextLength)
	}

	// Read the full metadata (arrays) needed to price iSWA.
	full := cheap
	if source.kind == sourceLocal {
		if f2, err := ggufparser.ParseGGUFFile(source.path, ggufparser.UseMMap()); err == nil {
			full = f2
		}
	}

	// Independent K/V (M4.1): price the sliding/global layers with the ACTUAL -ctk and -ctv
	// dtypes. Reading opts.KVCacheType here used to receive the canonical calibration key
	// ("q8_0/q4_0") for an asymmetric profile, which is not a dtype and fell through to f16.
	floorKType, floorVType := resolveKVPair(opts)
	floor, nSWA, nGlobal, desc, fok := iswaKVFloorBytes(full, effCtx, floorKType, floorVType)
	if !fok {
		return 0, 0, "", false
	}

	parserKV := parserKVBytes(run)
	if floor <= parserKV {
		// Library already priced the KV at least as high as our floor — leave it.
		return 0, 0, "", false
	}
	add := floor - parserKV
	note = fmt.Sprintf("SWA correction: %s under-priced this iSWA model's KV cache (%.0f MiB) — %s at ctx=%d "+
		"(%d sliding + %d global of %d layers, n_swa=%d) prices ~%.0f MiB; added +%.0f MiB (conservative, no false-fit OOM)",
		ParserVersion, float64(parserKV)/MiB, desc, effCtx, nSWA, nGlobal, nSWA+nGlobal, a.AttentionSlidingWindow,
		float64(floor)/MiB, float64(add)/MiB)
	return add, int(floor / MiB), note, true
}

// parserKVBytes sums the library's own KV-cache figure across all devices.
func parserKVBytes(run ggufparser.LLaMACppRunEstimate) uint64 {
	var b uint64
	for _, d := range run.Devices {
		b += uint64(d.KVCache.Key) + uint64(d.KVCache.Value)
	}
	return b
}

// iswaKVFloorBytes computes an iSWA-aware KV-cache floor in bytes for an SWA model
// at ctxLen, reading per-layer geometry from the GGUF metadata. ok is false when
// the model is not an SWA model (no <arch>.attention.sliding_window key) or the
// metadata needed to price it is absent/insufficient — in which case the caller
// leaves the library estimate untouched. desc is a short human note for verdicts.
// kType/vType are the INDEPENDENT key/value cache dtypes (llama.cpp -ctk/-ctv); each half of
// every layer's cache is priced with its own dtype (M4.1).
func iswaKVFloorBytes(f *ggufparser.GGUFFile, ctxLen int, kType, vType KVCacheType) (bytes uint64, nSWALayers, nGlobalLayers int, desc string, ok bool) {
	if f == nil || ctxLen <= 0 {
		return 0, 0, 0, "", false
	}
	m := metadataByKey(f)
	arch, aok := m["general.architecture"]
	if !aok {
		return 0, 0, 0, "", false
	}
	prefix := ValueString(arch)
	swKV, sok := m[prefix+".attention.sliding_window"]
	if !sok {
		return 0, 0, 0, "", false // not an SWA model
	}
	nSWA := int(ggufparser.ValueNumeric[uint64](swKV))
	if nSWA <= 0 {
		return 0, 0, 0, "", false
	}

	blockCount := 0
	if v, okk := m[prefix+".block_count"]; okk {
		blockCount = int(ggufparser.ValueNumeric[uint64](v))
	}
	if blockCount <= 0 {
		return 0, 0, 0, "", false
	}

	// Head dims: global (key_length/value_length) and sliding (…_swa, falling back
	// to the global dims when a model does not split them).
	kGlobal := metaUint(m, prefix+".attention.key_length", 0)
	vGlobal := metaUint(m, prefix+".attention.value_length", 0)
	kSWA := metaUint(m, prefix+".attention.key_length_swa", kGlobal)
	vSWA := metaUint(m, prefix+".attention.value_length_swa", vGlobal)
	if kGlobal == 0 || vGlobal == 0 {
		return 0, 0, 0, "", false
	}

	// Per-layer KV-head counts (Gemma-4: array; other models: scalar).
	hkv := perLayerUint(m, prefix+".attention.head_count_kv", blockCount)
	if len(hkv) == 0 {
		return 0, 0, 0, "", false
	}
	// Per-layer sliding mask (true => sliding/local layer). Gemma-4 stores a bool
	// array; a scalar integer pattern N is interpreted as "every Nth layer global".
	isSliding := perLayerSlidingMask(m, prefix+".attention.sliding_window_pattern", blockCount)

	// Bytes-per-element ×8 per SIDE (avoids float; conservative round-up). K and V are priced
	// separately so -ctk q8_0 -ctv q4_0 is not collapsed into one shared cost.
	bpeK := kvBytesPerElemX8(kType)
	bpeV := kvBytesPerElemX8(vType)
	swaTokens := ctxLen
	if nSWA+swaBatchAllowance < swaTokens {
		swaTokens = nSWA + swaBatchAllowance
	}

	var total uint64
	for i := 0; i < blockCount; i++ {
		h := hkv[min(i, len(hkv)-1)]
		if h == 0 {
			continue
		}
		var kElemsPerTok, vElemsPerTok, tokens uint64
		if isSliding[i] {
			kElemsPerTok, vElemsPerTok = kSWA*h, vSWA*h
			tokens = uint64(swaTokens)
			nSWALayers++
		} else {
			kElemsPerTok, vElemsPerTok = kGlobal*h, vGlobal*h
			tokens = uint64(ctxLen)
			nGlobalLayers++
		}
		// bytes = elems * tokens * (bpe/8); bpe is ×8 so divide once at the end.
		total += tokens * (kElemsPerTok*bpeK + vElemsPerTok*bpeV)
	}
	total /= 8

	if nSWALayers == 0 {
		// No layer was actually sliding — the model is not iSWA in practice; let the
		// library estimate stand (avoids second-guessing a correctly-priced model).
		return 0, 0, 0, "", false
	}
	return total, nSWALayers, nGlobalLayers, "iSWA-aware KV floor (window-capped sliding layers + full-ctx global layers)", true
}

// metadataByKey indexes a parsed GGUF's metadata by key.
func metadataByKey(f *ggufparser.GGUFFile) map[string]ggufparser.GGUFMetadataKV {
	out := make(map[string]ggufparser.GGUFMetadataKV, len(f.Header.MetadataKV))
	for _, kv := range f.Header.MetadataKV {
		out[kv.Key] = kv
	}
	return out
}

// ValueString reads a string metadata value (empty if not a string).
func ValueString(kv ggufparser.GGUFMetadataKV) string {
	if s, ok := kv.Value.(string); ok {
		return s
	}
	return ""
}

func metaUint(m map[string]ggufparser.GGUFMetadataKV, key string, dflt uint64) uint64 {
	if v, ok := m[key]; ok {
		return ggufparser.ValueNumeric[uint64](v)
	}
	return dflt
}

// perLayerUint returns a per-layer slice from a metadata key that may be a scalar
// (broadcast to all layers) or an array (used element-wise). Empty if absent.
func perLayerUint(m map[string]ggufparser.GGUFMetadataKV, key string, n int) []uint64 {
	v, ok := m[key]
	if !ok {
		return nil
	}
	if v.ValueType == ggufparser.GGUFMetadataValueTypeArray {
		arr := ggufparser.ValuesNumeric[uint64](v.ValueArray())
		if len(arr) == 0 {
			return nil
		}
		return arr
	}
	scalar := ggufparser.ValueNumeric[uint64](v)
	out := make([]uint64, n)
	for i := range out {
		out[i] = scalar
	}
	return out
}

// perLayerSlidingMask returns a per-layer bool mask (true => sliding/local layer).
// Gemma-4 stores a bool array (true=sliding). A scalar integer pattern N means
// "every Nth layer is GLOBAL" (i%N==0 => global), matching gguf-parser-go's own
// convention. When the key is absent, every layer is treated as sliding EXCEPT it
// is only reached for known-SWA models, so a missing pattern is the conservative
// "all sliding" (smaller KV) — callers only apply the floor when it EXCEEDS the
// library number, so this cannot cause an under-price.
func perLayerSlidingMask(m map[string]ggufparser.GGUFMetadataKV, key string, n int) []bool {
	out := make([]bool, n)
	v, ok := m[key]
	if !ok {
		for i := range out {
			out[i] = true
		}
		return out
	}
	if v.ValueType == ggufparser.GGUFMetadataValueTypeArray {
		arr := v.ValueArray()
		if arr.Type == ggufparser.GGUFMetadataValueTypeBool {
			bs := arr.ValuesBool()
			for i := range out {
				if i < len(bs) {
					out[i] = bs[i]
				}
			}
			return out
		}
		// Numeric array: nonzero => sliding.
		ns := ggufparser.ValuesNumeric[uint64](arr)
		for i := range out {
			if i < len(ns) && ns[i] != 0 {
				out[i] = true
			}
		}
		return out
	}
	// Scalar integer pattern N: i%N==0 => global, else sliding.
	patN := int(ggufparser.ValueNumeric[uint64](v))
	for i := range out {
		if patN <= 1 || i%patN != 0 {
			out[i] = true
		}
	}
	return out
}

// kvBytesPerElemX8 returns bytes-per-KV-element multiplied by 8 (to keep integer
// math while allowing fractional quantized sizes). f16 = 2 B; quantized values are
// rounded UP (q8_0 ≈ 1.06 → 1.125, q4_0 ≈ 0.56 → 0.625) so the floor stays
// conservative for a quantized KV cache.
func kvBytesPerElemX8(t KVCacheType) uint64 {
	switch t {
	case KVCacheQ8_0:
		return 9 // 1.125 * 8
	case KVCacheQ4_0:
		return 5 // 0.625 * 8
	case KVCacheF16, "":
		return 16 // 2.0 * 8
	default:
		return 16
	}
}
