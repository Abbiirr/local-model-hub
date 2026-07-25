package gguf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// fallback.go implements the REQUIRED low-confidence heuristic for GGUF files
// that gguf-parser-go cannot parse. The P0.2 field trial proved gguf-parser
// v0.24.1 aborts on TQ3_* quants with "GGMLType(46) ... unsupported" — even
// metadata read fails inside the library because it reads tensor info eagerly.
// Those TQ3 models are the PRIMARY workload of the reference box, so we must
// degrade gracefully instead of reporting "unknown/won't-run" (friction #1).
//
// ADR-002 boundary (documented deliberately):
//   - Reading the GGUF *header + KV metadata* ourselves (n_layer, n_embd_head,
//     n_head_kv, context_length) is METADATA READING, not estimation. It is
//     unavoidable here because the library refuses the whole file. We stop
//     BEFORE the tensor-info section (where the unknown GGMLType lives), so this
//     reader never touches quantized tensor types.
//   - The actual VRAM number is a simple, transparent, clearly-labelled formula
//     (weights ≈ file_size × 1.05 + KV(from header metadata + ctx) + ~500 MiB).
//     This is the labelled fallback ADR-002 explicitly permits, NOT a
//     replacement estimator for parseable files.
//
// Confidence on this path is always ConfidenceFallback and Notes always states
// why the fallback fired and which inputs it used.

// fallbackEstimate produces a fallback Estimate for a source that gguf-parser-go
// could not parse. parseErr is the library error (carried into Notes so the
// verdict can explain the low confidence). Remote sources cannot use the
// header-metadata path (we only have the local reader), so they degrade to the
// file-size-only formula with a loud note if size is unknown.
func fallbackEstimate(source Source, opts Options, parseErr error) (Result, error) {
	notes := []string{
		fmt.Sprintf("FALLBACK: gguf-parser-go (%s) could not parse this file: %v. "+
			"Using a conservative file-size + header-metadata heuristic instead of the library estimator "+
			"(e.g. TQ3_* custom quants report GGMLType(46) unsupported).", ParserVersion, parseErr),
	}

	// The header-metadata fallback path only applies to local files (we have a
	// raw reader for those). Remote sources fall through to file-size-only,
	// which requires a known size — otherwise it is unresolvable.
	if source.kind != sourceLocal {
		notes = append(notes, "LOUD NOTE: source is remote and unparseable; no local bytes to size or read header from — cannot produce even a file-size fallback. Treat as UNKNOWN and require a manual check or a local copy.")
		return Result{}, fmt.Errorf("gguf: cannot estimate unparseable remote source %s: %w", source.describe(), parseErr)
	}

	fi, statErr := os.Stat(source.path)
	if statErr != nil {
		notes = append(notes, fmt.Sprintf("LOUD NOTE: could not stat file (%v); no size available for even a file-size fallback.", statErr))
		return Result{}, fmt.Errorf("gguf: fallback stat failed for %s: %w", source.path, statErr)
	}
	fileSize := fi.Size()

	// Weights term: on-disk size × small overhead. This over-estimates slightly
	// (safe direction) and is the load-bearing term for large quantized weights.
	weightsMiB := int(math.Ceil(float64(fileSize) * fallbackWeightsOverhead / float64(MiB)))

	est := Result{
		Confidence:    ConfidenceFallback,
		ParserVersion: ParserVersion + "+fallback-heuristic",
	}

	// Try to read the GGUF header KV metadata for a real KV-cache term. This is
	// best-effort: if the header is unreadable we fall back to weights + fixed
	// overhead only, with a loud note.
	hdr, hdrErr := readGGUFHeaderMetadata(source.path)
	if hdrErr != nil {
		notes = append(notes,
			fmt.Sprintf("LOUD NOTE: GGUF header/KV metadata unreadable (%v). Using file-size-only formula: "+
				"VRAM ≈ file_size×%.2f + %d MiB fixed overhead. KV cache NOT modelled — real footprint grows with context; treat as a floor.",
				hdrErr, fallbackWeightsOverhead, fallbackFixedOverheadMiB))
		est.VRAMMiB = weightsMiB + fallbackFixedOverheadMiB
		est.RAMMiB = fallbackFixedOverheadMiB // minimal host reserve; weights are on GPU/mmap
		est.Notes = append(notes, fmt.Sprintf("inputs: file_size=%d bytes (%d MiB), weights≈%d MiB, overhead=%d MiB",
			fileSize, int(fileSize/MiB), weightsMiB, fallbackFixedOverheadMiB))
		return est, nil
	}

	est.Architecture = hdr.architecture

	// KV cache term from header metadata + requested context. This mirrors the
	// standard llama.cpp KV formula but is GQA-aware (uses n_head_kv, not
	// n_attention_heads) — the compat spec §4 warns the naive formula
	// over-estimates by the GQA ratio.
	ctx := opts.ContextLen
	if ctx <= 0 {
		if hdr.contextLength > 0 {
			ctx = int(hdr.contextLength)
		} else {
			ctx = 4096 // conservative default when the model's max ctx is unknown
			notes = append(notes, "note: model max context unknown from header; assumed 4096 for the KV term")
		}
	}
	kvBytesPerElem := kvBytesForType(opts.KVCacheType)
	kvBytes := kvCacheBytes(hdr, ctx, parallel(opts.ParallelSeq), kvBytesPerElem)
	kvMiB := int(math.Ceil(float64(kvBytes) / float64(MiB)))

	est.VRAMMiB = weightsMiB + kvMiB + fallbackFixedOverheadMiB
	est.CtxVRAMMiB = kvMiB
	est.RAMMiB = fallbackFixedOverheadMiB
	est.Layers = int(hdr.blockCount)

	notes = append(notes, fmt.Sprintf(
		"inputs: file_size=%d bytes (%d MiB) → weights≈%d MiB; header n_layer=%d, n_embd=%d, n_head=%d, n_head_kv=%d, head_dim=%d → KV≈%d MiB at ctx=%d, parallel=%d, kv_bytes/elem=%d; fixed overhead=%d MiB",
		fileSize, int(fileSize/MiB), weightsMiB,
		hdr.blockCount, hdr.embeddingLength, hdr.headCount, hdr.headCountKV, hdr.headDim(),
		kvMiB, ctx, parallel(opts.ParallelSeq), kvBytesPerElem, fallbackFixedOverheadMiB))
	est.Notes = notes
	return est, nil
}

// kvBytesForType returns per-element KV byte cost for a KV cache dtype on the
// fallback path. Empty => the conservative f16 default (2 bytes).
func kvBytesForType(t KVCacheType) int {
	switch t {
	case KVCacheF16, "":
		return fallbackKVDefaultBytesPerElem // 2
	case KVCacheQ8_0:
		return 1 // ~1 byte/elem effective (q8_0 block overhead ignored, conservative-low is acceptable here vs f16 default)
	case KVCacheQ4_0:
		return 1 // q4_0 is <1 byte/elem; round up to 1 to stay conservative
	default:
		return fallbackKVDefaultBytesPerElem
	}
}

// kvCacheBytes computes the KV cache footprint (GQA-aware) from header metadata.
//
//	kv_bytes = 2 (K and V) × n_layers × n_head_kv × head_dim × ctx × parallel × bytes_per_elem
func kvCacheBytes(h ggufHeaderMeta, ctx, parallelSeq, bytesPerElem int) uint64 {
	if parallelSeq <= 0 {
		parallelSeq = 1
	}
	headDim := h.headDim()
	if headDim == 0 || h.blockCount == 0 {
		return 0
	}
	nKV := h.headCountKV
	if nKV == 0 {
		nKV = h.headCount // MHA fallback: n_kv_heads == n_heads
	}
	if nKV == 0 {
		return 0
	}
	// 2 (K+V) × layers × n_kv_heads × head_dim × ctx × parallel × bytes/elem
	return 2 *
		uint64(h.blockCount) *
		uint64(nKV) *
		uint64(headDim) *
		uint64(ctx) *
		uint64(parallelSeq) *
		uint64(bytesPerElem)
}

// ggufHeaderMeta is the minimal subset of GGUF KV metadata the fallback needs.
type ggufHeaderMeta struct {
	architecture    string
	blockCount      uint64 // <arch>.block_count  (== n_layer)
	embeddingLength uint64 // <arch>.embedding_length (== hidden_size / n_embd)
	headCount       uint64 // <arch>.attention.head_count (== n_attention_heads)
	headCountKV     uint64 // <arch>.attention.head_count_kv (== n_kv_heads; 0 => MHA)
	contextLength   uint64 // <arch>.context_length (trained max ctx)
}

// headDim returns embedding_length / head_count, or 0 if not derivable.
func (h ggufHeaderMeta) headDim() uint64 {
	if h.headCount == 0 {
		return 0
	}
	return h.embeddingLength / h.headCount
}

// --- Minimal raw GGUF header/KV metadata reader (fallback inputs only) --------
//
// GGUF v2/v3 layout (little-endian) we read:
//   magic uint32 ("GGUF") | version uint32 | tensor_count uint64 | kv_count uint64
//   then kv_count metadata KV entries: key(string) + value_type(uint32) + value
// We read keys we care about and skip the rest, and we STOP after the KV block —
// we never parse tensor info, which is where TQ3's GGMLType(46) would blow up.

const ggufMagicLE = 0x46554747 // "GGUF" little-endian

var errNotGGUF = errors.New("not a GGUF file (bad magic)")

func readGGUFHeaderMetadata(path string) (ggufHeaderMeta, error) {
	var out ggufHeaderMeta
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	r := &leReader{r: f}

	magic, err := r.u32()
	if err != nil {
		return out, err
	}
	if magic != ggufMagicLE {
		return out, errNotGGUF
	}
	version, err := r.u32()
	if err != nil {
		return out, err
	}
	if version < 2 || version > 3 {
		// v1 used uint32 counts; we only support v2/v3 (the ubiquitous versions).
		return out, fmt.Errorf("unsupported GGUF version %d", version)
	}
	if _, err = r.u64(); err != nil { // tensor_count (skip)
		return out, err
	}
	kvCount, err := r.u64()
	if err != nil {
		return out, err
	}

	raw := map[string]any{}
	for i := uint64(0); i < kvCount; i++ {
		key, err := r.gstring()
		if err != nil {
			return out, fmt.Errorf("kv[%d] key: %w", i, err)
		}
		val, err := r.value()
		if err != nil {
			return out, fmt.Errorf("kv[%d] %q value: %w", i, key, err)
		}
		raw[key] = val
	}

	out.architecture, _ = raw["general.architecture"].(string)
	arch := out.architecture
	out.blockCount = kvUint(raw, arch+".block_count")
	out.embeddingLength = kvUint(raw, arch+".embedding_length")
	out.headCount = kvUint(raw, arch+".attention.head_count")
	out.headCountKV = kvUint(raw, arch+".attention.head_count_kv")
	out.contextLength = kvUint(raw, arch+".context_length")
	return out, nil
}

// kvUint coerces a stored metadata value to uint64 (metadata ints come in
// several widths). Returns 0 if absent or non-numeric.
func kvUint(m map[string]any, key string) uint64 {
	switch v := m[key].(type) {
	case uint8:
		return uint64(v)
	case int8:
		return uint64(v)
	case uint16:
		return uint64(v)
	case int16:
		return uint64(v)
	case uint32:
		return uint64(v)
	case int32:
		return uint64(v)
	case uint64:
		return v
	case int64:
		return uint64(v)
	default:
		return 0
	}
}

// leReader reads little-endian GGUF primitives.
type leReader struct {
	r io.Reader
}

func (lr *leReader) read(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(lr.r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (lr *leReader) u32() (uint32, error) {
	b, err := lr.read(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (lr *leReader) u64() (uint64, error) {
	b, err := lr.read(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// gstring reads a GGUF string: uint64 length prefix + raw bytes.
func (lr *leReader) gstring() (string, error) {
	n, err := lr.u64()
	if err != nil {
		return "", err
	}
	if n > 1<<20 { // 1 MiB sanity cap on a single metadata string
		return "", fmt.Errorf("gguf string too long: %d", n)
	}
	b, err := lr.read(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// GGUF metadata value type enum (matches gguf-parser-go / the GGUF spec).
const (
	mvUint8 uint32 = iota
	mvInt8
	mvUint16
	mvInt16
	mvUint32
	mvInt32
	mvFloat32
	mvBool
	mvString
	mvArray
	mvUint64
	mvInt64
	mvFloat64
)

// value reads one metadata value (leading value_type uint32).
func (lr *leReader) value() (any, error) {
	t, err := lr.u32()
	if err != nil {
		return nil, err
	}
	return lr.valueOfType(t)
}

func (lr *leReader) valueOfType(t uint32) (any, error) {
	switch t {
	case mvUint8:
		b, err := lr.read(1)
		if err != nil {
			return nil, err
		}
		return b[0], nil
	case mvInt8:
		b, err := lr.read(1)
		if err != nil {
			return nil, err
		}
		return int8(b[0]), nil
	case mvUint16:
		b, err := lr.read(2)
		if err != nil {
			return nil, err
		}
		return binary.LittleEndian.Uint16(b), nil
	case mvInt16:
		b, err := lr.read(2)
		if err != nil {
			return nil, err
		}
		return int16(binary.LittleEndian.Uint16(b)), nil
	case mvUint32:
		v, err := lr.u32()
		return v, err
	case mvInt32:
		v, err := lr.u32()
		return int32(v), err
	case mvFloat32:
		v, err := lr.u32()
		return math.Float32frombits(v), err
	case mvBool:
		b, err := lr.read(1)
		if err != nil {
			return nil, err
		}
		return b[0] != 0, nil
	case mvString:
		return lr.gstring()
	case mvUint64:
		v, err := lr.u64()
		return v, err
	case mvInt64:
		v, err := lr.u64()
		return int64(v), err
	case mvFloat64:
		v, err := lr.u64()
		return math.Float64frombits(v), err
	case mvArray:
		return lr.skipArray()
	default:
		return nil, fmt.Errorf("unknown metadata value type %d", t)
	}
}

// skipArray reads and discards an array value (element type + count + elements).
// We do not use any array-typed metadata for the fallback, so we only need to
// consume it to keep the stream aligned for later keys.
func (lr *leReader) skipArray() (any, error) {
	elemType, err := lr.u32()
	if err != nil {
		return nil, err
	}
	n, err := lr.u64()
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < n; i++ {
		if _, err := lr.valueOfType(elemType); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
