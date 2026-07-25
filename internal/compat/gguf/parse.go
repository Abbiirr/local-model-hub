package gguf

import (
	"context"
	"fmt"

	ggufparser "github.com/gpustack/gguf-parser-go"
)

// parseAndEstimate parses the GGUF via gguf-parser-go and returns an approximate
// estimate. It returns an error if the library cannot parse the file (the caller
// then routes to the fallback). All estimation math lives inside the library
// (ADR-002); this function only translates Options into the library's option
// functions and reads the NONUMA figures back out.
func parseAndEstimate(ctx context.Context, source Source, opts Options) (Result, error) {
	f, err := parseSource(ctx, source)
	if err != nil {
		return Result{}, err
	}

	runOpts, kvErr := runOptions(opts)
	if kvErr != nil {
		return Result{}, kvErr
	}

	run := f.EstimateLLaMACppRun(runOpts...)
	// Summarize with mmap=true (weights mmap'd from disk, the llama.cpp default)
	// and zero extra host/device footprint reservation — we surface the raw
	// per-device NONUMA figures and let residency add its own additive margin
	// (PLAN.md conflict-resolution #9: margins are applied downstream, not here).
	sum := run.Summarize(true, 0, 0)
	if len(sum.Items) == 0 {
		return Result{}, fmt.Errorf("gguf: estimator returned no summary items for %s", source.describe())
	}
	item := sum.Items[0]

	// VRAM: sum NONUMA across all GPU devices. On this single-GPU box that is
	// one device; summing keeps it correct if a split is ever configured.
	var vramBytes uint64
	for _, v := range item.VRAMs {
		vramBytes += uint64(v.NonUMA)
	}
	ramBytes := uint64(item.RAM.NonUMA)

	notes := []string{
		fmt.Sprintf("estimated by %s (arch=%s) at ctx=%d, flash_attn=%v, kv=%s, ngl=%s, parallel=%d",
			ParserVersion, sum.Architecture, sum.ContextSize, opts.FlashAttention,
			kvLabel(opts.KVCacheType), nglLabel(opts.NGL), parallel(opts.ParallelSeq)),
	}
	if run.Projector != nil {
		notes = append(notes, "note: multimodal projector (mmproj) present; its VRAM is included by the estimator only if you passed it explicitly — a sibling mmproj file loaded at runtime adds VRAM not counted here")
	}

	return Result{
		VRAMMiB:       bytesToMiB(vramBytes),
		RAMMiB:        bytesToMiB(ramBytes),
		CtxVRAMMiB:    0, // library summary does not cleanly separate KV from weights per device; left 0.
		Layers:        int(item.OffloadLayers),
		Confidence:    ConfidenceApproximate,
		Notes:         notes,
		ParserVersion: ParserVersion,
		Architecture:  sum.Architecture,
	}, nil
}

// parseSource dispatches to the correct gguf-parser-go parse entry point. For
// remote sources the library issues HTTP range requests to read only the GGUF
// header + tensor-info section (no weight download); the context (bounded by
// Options.RemoteTimeout via the caller) caps how long that can take.
func parseSource(ctx context.Context, source Source) (*ggufparser.GGUFFile, error) {
	switch source.kind {
	case sourceLocal:
		return ggufparser.ParseGGUFFile(source.path)
	case sourceHF:
		var readOpts []ggufparser.GGUFReadOption
		if source.revision != "" {
			// Encode a pinned revision into the repo ref the library resolves.
			// gguf-parser-go accepts "repo@revision" for HF sources.
			return ggufparser.ParseGGUFFileFromHuggingFace(ctx, source.repo+"@"+source.revision, source.file, readOpts...)
		}
		return ggufparser.ParseGGUFFileFromHuggingFace(ctx, source.repo, source.file, readOpts...)
	case sourceURL:
		return ggufparser.ParseGGUFFileRemote(ctx, source.url)
	default:
		return nil, ErrEmptySource
	}
}

// runOptions translates Options into gguf-parser-go run-estimate options.
func runOptions(opts Options) ([]ggufparser.GGUFRunEstimateOption, error) {
	var out []ggufparser.GGUFRunEstimateOption

	// Context size: 0 => model's trained maximum.
	if opts.ContextLen > 0 {
		out = append(out, ggufparser.WithLLaMACppContextSize(int32(opts.ContextLen)))
	} else {
		out = append(out, ggufparser.WithinLLaMACppMaxContextSize())
	}

	if opts.FlashAttention {
		out = append(out, ggufparser.WithFlashAttention())
	}

	// KV cache dtype (both key and value set to the same type, matching how
	// llama.cpp's -ctk/-ctv are usually paired for the standard quants).
	if opts.KVCacheType != "" {
		t, err := kvGGMLType(opts.KVCacheType)
		if err != nil {
			return nil, err
		}
		out = append(out,
			ggufparser.WithLLaMACppCacheKeyType(t),
			ggufparser.WithLLaMACppCacheValueType(t),
		)
	}

	// NGL: -1 or 0 => offload all layers (a large sentinel offloads every layer;
	// the library clamps to the actual layer count).
	if opts.NGL > 0 {
		out = append(out, ggufparser.WithLLaMACppOffloadLayers(uint64(opts.NGL)))
	} else {
		out = append(out, ggufparser.WithLLaMACppOffloadLayers(1<<20))
	}

	if opts.ParallelSeq > 0 {
		out = append(out, ggufparser.WithParallelSize(int32(opts.ParallelSeq)))
	}

	return out, nil
}

// kvGGMLType maps our KVCacheType to the library's GGMLType constant.
func kvGGMLType(t KVCacheType) (ggufparser.GGMLType, error) {
	switch t {
	case KVCacheF16:
		return ggufparser.GGMLTypeF16, nil
	case KVCacheQ8_0:
		return ggufparser.GGMLTypeQ8_0, nil
	case KVCacheQ4_0:
		return ggufparser.GGMLTypeQ4_0, nil
	default:
		return 0, fmt.Errorf("gguf: unsupported KV cache type %q (want one of f16|q8_0|q4_0)", t)
	}
}

func bytesToMiB(b uint64) int { return int(b / MiB) }

func kvLabel(t KVCacheType) string {
	if t == "" {
		return "f16(default)"
	}
	return string(t)
}

func nglLabel(ngl int) string {
	if ngl <= 0 {
		return "all"
	}
	return fmt.Sprintf("%d", ngl)
}

func parallel(p int) int {
	if p <= 0 {
		return 1
	}
	return p
}
