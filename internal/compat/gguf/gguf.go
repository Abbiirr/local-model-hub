// Package gguf provides GGUF VRAM/RAM estimation for LocalHub Compass by
// WRAPPING the gpustack/gguf-parser-go library (ADR-002: do not hand-write a
// GGUF estimator). All estimation math is delegated to gguf-parser-go v0.24.1;
// the only local math is a clearly-labelled *fallback* heuristic used when the
// library cannot parse a file (e.g. the TQ3_* custom quants this box depends on,
// which fail with "GGMLType(46) ... unsupported"). See fallback.go for the
// boundary between "reading GGUF header metadata" (allowed) and "estimating"
// (delegated).
//
// Library version wrapped: github.com/gpustack/gguf-parser-go v0.24.1
// Parse entry points used:
//   - ParseGGUFFile(path, ...GGUFReadOption)                          (local file)
//   - ParseGGUFFileFromHuggingFace(ctx, repo, file, ...GGUFReadOption)(remote HF, range reads)
//   - ParseGGUFFileRemote(ctx, url, ...GGUFReadOption)                (remote direct URL, range reads)
//
// Estimate entry point used:
//   - (*GGUFFile).EstimateLLaMACppRun(...GGUFRunEstimateOption) -> LLaMACppRunEstimate
//   - (LLaMACppRunEstimate).Summarize(mmap, ramFootprint, vramFootprint) -> summary
//     from which we read Items[0].VRAMs[].NonUMA (the "NONUMA VRAM" figure that
//     matters on Windows/non-UMA, per research/phase-0-friction-log.md) and
//     Items[0].RAM.NonUMA.
//
// Run options used: WithLLaMACppContextSize, WithFlashAttention,
//
//	WithLLaMACppCacheKeyType / WithLLaMACppCacheValueType (KV cache dtype),
//	WithLLaMACppOffloadLayers (NGL), WithParallelSize (concurrent sequences).
package gguf

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MiB is one mebibyte in bytes.
const MiB = 1024 * 1024

// Fallback / heuristic constants. These are the ONLY numeric estimation
// constants in this package that are not produced by gguf-parser-go, and they
// are used exclusively on the fallback path (unparseable quant types). They are
// deliberately conservative (over-estimate) because a false "fits" that OOMs is
// a blocking correctness bug (CLAUDE.md correctness bar).
const (
	// fallbackWeightsOverhead multiplies the on-disk file size to approximate
	// resident weight VRAM. Weights load roughly 1:1 from disk; the extra ~5%
	// covers alignment/padding and small per-tensor overhead.
	fallbackWeightsOverhead = 1.05

	// fallbackFixedOverheadMiB is a fixed CUDA-context + compute-buffer reserve
	// added on the fallback path, in MiB. ~500 MiB matches the small fixed
	// overhead observed for llama.cpp full-offload loads on the reference box.
	fallbackFixedOverheadMiB = 500

	// fallbackKVDefaultBytesPerElem is the per-element KV byte cost assumed on
	// the fallback path when the caller did not pin a KV cache type. f16 KV = 2
	// bytes; we default conservatively to 2.
	fallbackKVDefaultBytesPerElem = 2
)

// Confidence classifies how much trust to place in an estimate.
type Confidence string

const (
	// ConfidenceExact means the number is authoritative: either a measured
	// observed peak from the calibration store, or (not used here) a value we
	// have otherwise verified against a real load.
	ConfidenceExact Confidence = "exact"
	// ConfidenceApproximate means gguf-parser-go parsed the file and produced
	// the estimate. This is the normal happy path.
	ConfidenceApproximate Confidence = "approximate"
	// ConfidenceFallback means gguf-parser-go could NOT parse the file and the
	// number came from the local file-size + header heuristic. Treat as a
	// lower bound on quality; Notes explains why and which inputs were used.
	ConfidenceFallback Confidence = "fallback"
)

// KVCacheType names a KV cache quantization understood by both llama.cpp and
// gguf-parser-go. Only the values below are accepted; anything else is an error
// from Estimate (we do not silently guess).
type KVCacheType string

const (
	KVCacheF16  KVCacheType = "f16"
	KVCacheQ8_0 KVCacheType = "q8_0"
	KVCacheQ4_0 KVCacheType = "q4_0"
)

// CanonicalKVKey renders an independent (K,V) pair as the SINGLE key the observed-peak
// calibration store is indexed by (model_key, ctx, kv_type). A symmetric pair renders as the
// bare type — "f16", "q4_0" — so every peak recorded before independent K/V existed still
// matches and calibration is not silently lost. An asymmetric pair renders as "k/v"
// (e.g. "q8_0/q4_0"), which correctly MISSES until a peak is measured for that exact pair:
// a measurement taken with a q4_0 value cache must never be reused to justify a q8_0 one.
// Empty sides resolve to f16, matching the estimator default.
func CanonicalKVKey(k, v KVCacheType) KVCacheType {
	if k == "" {
		k = KVCacheF16
	}
	if v == "" {
		v = KVCacheF16
	}
	if k == v {
		return k
	}
	return KVCacheType(string(k) + "/" + string(v))
}

// CalibrationKey returns the observed-peak lookup key for these options: the explicit
// Options.CalibKey when set, else CanonicalKVKey of the EFFECTIVE (K,V) pair. This is the ONLY
// place the calibration key is derived, so the estimator, the verdict, and the recording side
// cannot drift apart (the bug that silently degraded every verdict from `exact` to `fallback`).
func (o Options) CalibrationKey() KVCacheType {
	if o.CalibKey != "" {
		return o.CalibKey
	}
	k, v := resolveKVPair(o)
	return CanonicalKVKey(k, v)
}

// Options are the inputs P2.3's rule engine passes per (model, candidate-config).
type Options struct {
	// ContextLen is the context size in tokens to estimate at. 0 => the model's
	// trained maximum context (gguf-parser WithinLLaMACppMaxContextSize).
	ContextLen int
	// FlashAttention toggles flash-attention accounting (the reference box's
	// llama.cpp profiles run -fa on).
	FlashAttention bool
	// KVCacheType selects the KV cache dtype for BOTH key and value when the independent K/V
	// fields below are unset. Empty => f16 (gguf-parser default).
	KVCacheType KVCacheType
	// KVCacheTypeK / KVCacheTypeV select INDEPENDENT key/value cache dtypes (llama.cpp's
	// separate -ctk/-ctv). The profile PROVEN at 64K on the reference box is SYMMETRIC
	// -ctk q4_0 -ctv q4_0 (scripts/run-qwen-64k-q4q4.ps1); an asymmetric pair such as
	// -ctk q8_0 -ctv q4_0 is equally expressible and is what these independent fields exist for.
	// When set, each overrides KVCacheType for its side; when empty, that side falls back to
	// KVCacheType (then f16). Verified supported by b10106 --help (M2.x/M4.1).
	KVCacheTypeK KVCacheType
	KVCacheTypeV KVCacheType
	// CalibKey overrides the observed-peak calibration lookup key (the `kv_type` column of
	// observed_peaks). Leave EMPTY in normal use: the key is then derived as
	// CanonicalKVKey(effective K, effective V), which renders a symmetric pair as the bare type
	// ("q4_0") so rows recorded before independent K/V still match, and an asymmetric pair as
	// "q8_0/q4_0" — which correctly MISSES until measured for that exact pair.
	//
	// WHY this field exists (regression guard, KNOWN-LIMITATIONS #20): the calibration key used
	// to be smuggled through KVCacheType, which is ALSO a dtype consumed by the TQ3 file-size
	// fallback (kvBytesForType) and the iSWA floor (kvBytesPerElemX8). Writing "q8_0/q4_0" into a
	// dtype field made both fall through to their default branch and mis-price the KV cache while
	// printing a note that disagreed with the emitted -ctk/-ctv flags. Keeping the calibration KEY
	// and the DTYPES in separate fields makes that class of bug impossible.
	CalibKey KVCacheType
	// NGL is the number of layers to offload to GPU. -1 (or 0) => offload all
	// layers (full GPU offload, the reference-box policy). A positive value
	// offloads exactly that many.
	NGL int
	// ParallelSeq is the number of concurrent sequences (llama.cpp -np). 0 => 1.
	ParallelSeq int

	// RemoteTimeout bounds remote (HF/URL) metadata reads. 0 => defaultRemoteTimeout.
	// The library reads GGUF header + tensor-info via HTTP range requests only
	// (no full weight download); this timeout is the byte-budget proxy — a
	// misbehaving repo cannot drag on past it. See Source.
	RemoteTimeout time.Duration
}

// defaultRemoteTimeout bounds remote metadata reads when Options.RemoteTimeout==0.
const defaultRemoteTimeout = 30 * time.Second

// Result is the package's normalized estimate, returned by Estimate. All memory
// figures are in MiB. (Named Result, not Estimate, because Go forbids a type and
// a function sharing a name; the task spec's "Estimate = {…}" return type maps to
// this struct one-to-one.)
type Result struct {
	// VRAMMiB is the estimated GPU VRAM footprint (non-UMA) at the requested
	// context/config. This is the number residency admission and the compat
	// verdict consume.
	VRAMMiB int
	// RAMMiB is the estimated host RAM footprint (non-UMA).
	RAMMiB int
	// CtxVRAMMiB is the portion of VRAMMiB attributable to the KV cache /
	// context, when the underlying estimator exposes it; 0 if not available
	// (e.g. the fallback path where we cannot separate it cleanly).
	CtxVRAMMiB int
	// Layers is the number of layers offloaded to GPU in this estimate.
	Layers int
	// Confidence classifies the estimate (exact | approximate | fallback).
	Confidence Confidence
	// Notes carries human-readable explanation, especially WHY a fallback was
	// used and which inputs fed it. Always non-nil on a successful return.
	Notes []string
	// ParserVersion records the wrapped library version (or a fallback marker),
	// for the reproducibility / version-drift requirement (docs/22).
	ParserVersion string
	// Architecture is the model architecture string when known ("glm4",
	// "llama", ...); empty on the file-size-only fallback.
	Architecture string
}

// ParserVersion is the exact gguf-parser-go version this package wraps. Bump
// this string when the go.mod dependency is upgraded, and re-run the real-file
// tests (they pin wrapper correctness against recorded numbers).
const ParserVersion = "gguf-parser-go@v0.24.1"

// CalibrationStore lets Estimate prefer a measured observed peak over any
// computed estimate (the "measured-VRAM beats estimate" lesson from lmwrapper:
// research/lmwrapper-reuse-notes.md §3). Wiring lands later (P2.5): observed
// peaks are recorded in the DB during load validation and looked up by a stable
// model key. Tests use a fake.
type CalibrationStore interface {
	// ObservedPeak returns a measurement only when it was captured with the same
	// context length and KV cache type. A peak at a smaller context is never
	// evidence that a larger context fits.
	ObservedPeak(modelKey string, contextLen int, kvType KVCacheType) (mib int, ok bool)
}

// Source identifies where to read the GGUF metadata from. Construct one with
// LocalFile, HFRepo, or URL. It is an opaque tagged union.
type Source struct {
	kind     sourceKind
	path     string // LocalFile
	repo     string // HFRepo
	file     string // HFRepo
	revision string // HFRepo (optional; "" => library default / main)
	url      string // URL

	// modelKey is the calibration lookup key. Defaults to a source-derived
	// string; override with WithModelKey for a stable slug matching the DB.
	modelKey string
}

type sourceKind int

const (
	sourceLocal sourceKind = iota
	sourceHF
	sourceURL
)

// LocalFile is a Source reading a GGUF file from a local path.
func LocalFile(path string) Source {
	return Source{kind: sourceLocal, path: path, modelKey: path}
}

// HFRepo is a Source reading GGUF metadata from a Hugging Face repo via HTTP
// range requests (no full download). revision may be "" for the repo default.
func HFRepo(repo, file, revision string) Source {
	key := repo + "/" + file
	if revision != "" {
		key += "@" + revision
	}
	return Source{kind: sourceHF, repo: repo, file: file, revision: revision, modelKey: key}
}

// URL is a Source reading GGUF metadata from a direct blob URL via HTTP range
// requests (no full download).
func URL(u string) Source {
	return Source{kind: sourceURL, url: u, modelKey: u}
}

// WithModelKey overrides the calibration-store lookup key for this Source so it
// matches the model's stable slug in the DB. Returns a copy.
func (s Source) WithModelKey(key string) Source {
	s.modelKey = key
	return s
}

func (s Source) describe() string {
	switch s.kind {
	case sourceLocal:
		return "local:" + s.path
	case sourceHF:
		if s.revision != "" {
			return fmt.Sprintf("hf:%s/%s@%s", s.repo, s.file, s.revision)
		}
		return fmt.Sprintf("hf:%s/%s", s.repo, s.file)
	case sourceURL:
		return "url:" + s.url
	default:
		return "unknown"
	}
}

// ErrEmptySource is returned when a zero-value/invalid Source is passed.
var ErrEmptySource = errors.New("gguf: empty or invalid source")

// Estimate returns a normalized VRAM/RAM estimate for source under opts.
//
// Order of resolution:
//  1. If a CalibrationStore has an observed peak for the source's model key,
//     return it as an exact estimate (measured beats computed).
//  2. Parse via gguf-parser-go and produce an approximate estimate.
//  3. If parsing fails (unknown quant/GGML type, or unreadable), fall back to
//     the labelled heuristic (fallback.go) and return a fallback estimate. A
//     parse failure is NOT surfaced as an error to the caller — an unparseable
//     model must never be reported as "unknown/won't-run" (friction log #1).
//
// A non-nil error is returned only for input/config problems (bad source, bad
// KV type) or a total failure where even the fallback cannot produce a number.
func Estimate(ctx context.Context, source Source, opts Options) (Result, error) {
	return estimateWith(ctx, source, opts, nil)
}

// EstimateWithCalibration is Estimate with a calibration store consulted first.
// store may be nil (equivalent to Estimate).
func EstimateWithCalibration(ctx context.Context, source Source, opts Options, store CalibrationStore) (Result, error) {
	return estimateWith(ctx, source, opts, store)
}

func estimateWith(ctx context.Context, source Source, opts Options, store CalibrationStore) (Result, error) {
	if source.kind == sourceLocal && source.path == "" ||
		source.kind == sourceHF && (source.repo == "" || source.file == "") ||
		source.kind == sourceURL && source.url == "" {
		return Result{}, ErrEmptySource
	}

	// (1) Calibration override — measured VRAM beats any estimate. The lookup key is the
	// CANONICAL (K,V) key, never a raw dtype: a peak measured with a q4_0 value cache must not
	// justify a q8_0 one (KNOWN-LIMITATIONS #20).
	if store != nil {
		if mib, ok := store.ObservedPeak(source.modelKey, opts.ContextLen, opts.CalibrationKey()); ok {
			return Result{
				VRAMMiB:    mib,
				Confidence: ConfidenceExact,
				Notes: []string{
					fmt.Sprintf("measured observed VRAM peak %d MiB from calibration store (key=%q); "+
						"measured value preferred over estimate (lmwrapper measured-VRAM lesson)", mib, source.modelKey),
				},
				ParserVersion: "calibration/observed-peak",
			}, nil
		}
	}

	// For remote sources, bound how long the range-request metadata read may run.
	// gguf-parser-go fetches only the GGUF header + tensor-info via HTTP range
	// requests (no weight download), but a misbehaving repo could still stall; the
	// timeout is the byte-budget proxy that caps that.
	if source.kind == sourceHF || source.kind == sourceURL {
		timeout := opts.RemoteTimeout
		if timeout <= 0 {
			timeout = defaultRemoteTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// (2) Happy path: parse + estimate via gguf-parser-go.
	est, parseErr := parseAndEstimate(ctx, source, opts)
	if parseErr == nil {
		return est, nil
	}

	// (3) Fallback: the library could not parse this file (e.g. TQ3_* /
	// GGMLType(46)). Degrade to the labelled heuristic; never propagate the
	// parse error as a won't-run verdict.
	return fallbackEstimate(source, opts, parseErr)
}
