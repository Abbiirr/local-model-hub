# Wiring notes — P2.1 `internal/compat/gguf` (GGUF VRAM estimation)

Produced by the P2.1 implementer. Feeds P2.3 (rule engine), P2.4 (`/api/compat/preview`),
P2.5 (calibration / measured-peak loop). Package wraps **gguf-parser-go v0.24.1** as a
library (ADR-002). No hand-written GGUF estimator except the clearly-labelled fallback.

## Package API summary (`github.com/localhub/localhub/internal/compat/gguf`)

Entry points:

```go
func Estimate(ctx context.Context, source Source, opts Options) (Result, error)
func EstimateWithCalibration(ctx, source, opts, store CalibrationStore) (Result, error)
```

Sources (tagged union; construct with one of):

```go
LocalFile(path string) Source                       // local .gguf
HFRepo(repo, file, revision string) Source          // remote HF, range reads, no download
URL(u string) Source                                // remote blob URL, range reads
Source.WithModelKey(key string) Source              // sets calibration lookup key
```

Options (what P2.3 passes per candidate config):

```go
type Options struct {
    ContextLen     int           // tokens; 0 => model trained max ctx
    FlashAttention bool          // reference box runs -fa on
    KVCacheType    KVCacheType   // "" (f16) | "f16" | "q8_0" | "q4_0"
    NGL            int           // -1 or 0 => offload ALL layers (full GPU); >0 => that many
    ParallelSeq    int           // -np concurrent sequences; 0 => 1
    RemoteTimeout  time.Duration // bounds HF/URL metadata reads; 0 => 30s
}
```

Result (normalized; all memory in **MiB**):

```go
type Result struct {
    VRAMMiB       int          // GPU VRAM (non-UMA) — the number residency/verdict consumes
    RAMMiB        int          // host RAM (non-UMA)
    CtxVRAMMiB    int          // KV/context portion when derivable (fallback path only, 0 on parse path)
    Layers        int          // offloaded layer count
    Confidence    Confidence   // "exact" | "approximate" | "fallback"
    Notes         []string     // human-readable; ALWAYS explains fallback + inputs
    ParserVersion string       // "gguf-parser-go@v0.24.1" | "...+fallback-heuristic" | "calibration/observed-peak"
    Architecture  string       // "glm4","qwen35",... when known
}
```

> Naming note: the result struct is `Result`, not `Estimate`, because Go forbids a type
> and a function sharing a name. The task-spec field list ("Estimate = {…}") maps 1:1 to
> `Result`. Confidence levels use the spec's exact strings.

### gguf-parser-go API actually used (v0.24.1)

| Purpose | Call |
|---|---|
| Parse local | `ParseGGUFFile(path)` |
| Parse remote HF (range reads) | `ParseGGUFFileFromHuggingFace(ctx, repo[, "@rev"], file)` |
| Parse remote URL (range reads) | `ParseGGUFFileRemote(ctx, url)` |
| Estimate | `(*GGUFFile).EstimateLLaMACppRun(opts...) -> LLaMACppRunEstimate` |
| Read numbers | `.Summarize(mmap=true, 0, 0).Items[0].VRAMs[i].NonUMA` (VRAM), `.Items[0].RAM.NonUMA` (RAM) |

Run options mapped: `WithLLaMACppContextSize` / `WithinLLaMACppMaxContextSize`,
`WithFlashAttention`, `WithLLaMACppCacheKeyType`+`WithLLaMACppCacheValueType` (KV dtype,
both set together), `WithLLaMACppOffloadLayers` (NGL; `1<<20` sentinel = all, library
clamps), `WithParallelSize`.

We read **NONUMA** VRAM (the figure that matters on Windows/non-UMA, per the P0.2
friction log). `Summarize(true, 0, 0)` passes zero extra footprint reservation — the
package surfaces the raw estimator number and lets **residency add its own additive
margin downstream** (PLAN.md conflict-resolution #9: the estimator does not bake in the
admission margin).

## What P2.3's rule engine should pass

- **ctx candidates:** call `Estimate` once per context the verdict wants to report
  (e.g. the requested ctx AND the model max) to produce "fits at 8K, not 32K" — pass
  `ContextLen=8192` then `ContextLen=0` (max). `CtxVRAMMiB` is only populated on the
  fallback path today; for parseable models, derive the ctx sensitivity by diffing
  `VRAMMiB` across two ctx calls.
- **KV type from the engine profile:** the reference-box llama.cpp bat profiles use
  `-ctk q4_0 -ctv tq3_0`. gguf-parser-go only accepts up to `q4_0`/`q5_1` for KV value
  type, so `tq3_0` is NOT representable — pass `KVCacheType: KVCacheQ4_0` as the closest
  representable value and note in the verdict that the fork's actual `tq3_0` KV footprint
  is smaller (friction #9). For standard Ollama/winget runs pass `KVCacheF16` (default).
- **FlashAttention:** pass `true` for llama.cpp candidates (reference profiles run `-fa on`).
- **NGL:** pass `-1` (full offload) — the reference-box policy is full-GPU-offload-or-fail;
  never CPU offload for verification (PLAN.md hardware safeguard).
- **ParallelSeq:** default `1` on this box (the bat profiles use `-np 1`); higher only if
  the verdict is asked about concurrent serving.

The rule engine then compares `VRAMMiB` against **live free VRAM from the hardware probe**
(not nominal 16 GB — baseline desktop eats ~2.8 GB, friction #2) and applies its margin.

## Fallback semantics + confidence levels

Resolution order inside `Estimate`:

1. **Calibration hit** → `Confidence="exact"`, `VRAMMiB` = observed peak (measured beats
   estimate, lmwrapper lesson). Note says so.
2. **Parse succeeds** → `Confidence="approximate"`, number straight from gguf-parser-go.
3. **Parse fails** (unknown quant/GGML type, e.g. **TQ3_\*** → `GGMLType(46)`) → the
   library error is **NOT** propagated as won't-run. We degrade:
   - **Local file, header readable:** `Confidence="fallback"`.
     `VRAM ≈ file_size×1.05 (weights) + KV(GQA-aware, from header) + 500 MiB overhead`.
     Header KV metadata (`n_layer`, `n_embd`, `n_head`, `n_head_kv`, `context_length`) is
     read by a **minimal raw GGUF reader in `fallback.go`** that stops BEFORE the
     tensor-info section — so the unknown tensor type never blocks it. **ADR-002 boundary:
     this is metadata reading, not estimation logic** (documented in the file header).
     KV term is GQA-aware (uses `n_head_kv`, falls back to `n_head` for MHA).
   - **Local file, header unreadable:** file-size-only formula
     (`file_size×1.05 + 500 MiB`), `Confidence="fallback"`, **LOUD NOTE** that KV is not
     modelled and the number is a floor.
   - **Remote + unparseable:** returns an **error** (no local bytes to size / read header) —
     P2.3 should surface this as `unknown` with a reason, requiring a local copy or manual
     check. (Remote parseable models work fine; only unparseable-remote is unresolvable.)

`Notes` on every fallback states *why* (the exact library error incl. `GGMLType(46)`) and
*which inputs* fed the number (file size, header fields, ctx, KV bytes/elem, overhead).

Fallback constants (only non-library numbers, all conservative/over-estimate):
`fallbackWeightsOverhead=1.05`, `fallbackFixedOverheadMiB=500`, KV default `2 bytes/elem`
(f16), `q8_0`/`q4_0` → `1 byte/elem` (rounded up).

**Known-good recorded numbers (real-file tests, pinned):**
- GLM-OCR Q8_0 @4K → **1331 MiB** (library direct; P0.2 CLI recorded 1582, NVML-measured
  ~1105 — parser is conservative-over, correct direction). Test band [1000,1800].
- Qwen3.6-27B-TQ3_4S (13.95 GB file) → **fallback**, ~**14900 MiB** VRAM, arch=qwen35,
  n_layer=64, n_head_kv=4 (GQA read correctly). Test band [13000,16500].
- Remote bartowski Llama-3.2-1B-Instruct-GGUF Q4_K_M @8K → P0.2 recorded 1798 MiB
  (network-gated test, band [1200,2600]).

## Calibration-store wiring suggestion (P2.5)

```go
type CalibrationStore interface { ObservedPeak(modelKey string) (mib int, ok bool) }
```

- **When:** P2.5 records the measured NVML VRAM peak during load validation (the
  measured-VRAM-beats-estimate loop; friction #6, lmwrapper §3). Store it in the DB keyed
  by the model's stable slug.
- **Key:** pass the model slug via `Source.WithModelKey(slug)` so the lookup matches. By
  default the key is source-derived (path / `repo/file@rev` / url) — override it to the DB
  slug for stable calibration hits across sources.
- **Effect:** once a peak exists, `EstimateWithCalibration` returns it verbatim with
  `Confidence="exact"`, short-circuiting both parse and fallback. This is how the estimate
  self-corrects after the first real run (esp. important for TQ3 models where the parser is
  blind and the fallback is only a size heuristic).
- **Fake in tests:** `fakeCalibration{peaks: map[string]int{...}}` in `gguf_test.go`.

## Remote metadata read — what the library actually fetches

`ParseGGUFFileFromHuggingFace` / `ParseGGUFFileRemote` issue **HTTP range requests** to
read only the GGUF header + tensor-info section — **no weight download**. `Estimate`
wraps a `context.WithTimeout` around remote calls (`Options.RemoteTimeout`, default 30s)
as the byte-budget proxy so a misbehaving repo cannot stall the daemon. (The library also
exposes `UseBufferSize`, `UseBearerAuth`, `UseProxy`, `SkipTLSVerification` read options if
P3.x needs gated-repo auth or a proxy — not wired yet; add via a readOpts hook in
`parseSource` when needed.)

## Files

- `internal/compat/gguf/gguf.go` — public API (Source, Options, Result, Confidence,
  CalibrationStore, `Estimate`, `EstimateWithCalibration`), resolution order, remote timeout.
- `internal/compat/gguf/parse.go` — gguf-parser-go wrapper (parse dispatch + run options +
  NONUMA readout).
- `internal/compat/gguf/fallback.go` — labelled fallback heuristic + minimal raw GGUF
  header/KV metadata reader (ADR-002 boundary documented).
- `internal/compat/gguf/*_test.go` — synthetic-GGUF helper, fallback/calibration/validation
  unit tests, real-file tests (short-skip + missing-file-skip), network-gated remote test.
