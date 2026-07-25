# Wiring notes — P2.2 safetensors/vLLM estimator (`internal/compat/safetensors`)

Author: P2.2 PACKAGE agent. Audience: P2.3 (compat rule engine / verdict assembler), the
P4 vLLM adapter, and the orchestrator (assumptions to review). The package is complete,
vetted, and tested (unit + property). **No wiring was performed by this agent** (coordination
rule: only `internal/compat/safetensors/` + this file). GGUF lives in the sibling
`internal/compat/gguf/` (another agent) — not touched.

## 1. Package API (`github.com/localhub/localhub/internal/compat/safetensors`)

All VRAM is **MiB** (matches `internal/hardware` + verdict `vram_*_mb`; 1 MiB = 1024*1024 B).

**Config parsing**
- `func ParseConfig(raw []byte) (ModelConfig, error)` — defensive parse of `config.json`.
  Errors only on non-JSON; a sparse config parses and is validated in `EstimateVRAM`.
- `type ModelConfig struct{ HiddenSize, NumHiddenLayers, NumAttentionHeads int;
  NumKeyValueHeads *int; HeadDim *int; IntermediateSize, VocabSize int; TorchDtype string;
  MaxPositionEmbeds int; Architectures []string; ModelType string; TieWordEmbeddings *bool;
  QuantizationConfig *quantizationConfig }`
- Methods: `KVHeads() int` (absent num_key_value_heads => MHA => == num_attention_heads);
  `HeadDimEff() int` (prefers explicit `head_dim`, else `hidden_size/num_attention_heads`);
  `IsGQA() bool`; `GQARatio() float64` (n_attn/n_kv, >=1); `Quant() QuantMethod`
  (awq|gptq|fp8|none; "compressed-tensors" approximated by bit-width).

**Weights size (spec §4(1) preference ladder)**
- `func ParseIndexTotalSize(raw []byte) (WeightsInfo, bool)` — `model.safetensors.index.json`
  `metadata.total_size` (exact). **Rung 1.**
- `func WeightsFromShardSizes(shardSizes []int64) (WeightsInfo, bool)` — sum of per-shard
  sizes (e.g. from an HF API file listing). **Rung 2.**
- `func SumLocalSafetensorsHeaders(paths []string) (WeightsInfo, bool)` — parse the 8-byte
  length + JSON header of LOCAL `.safetensors` files, sum tensor-data sizes WITHOUT reading
  tensor bodies. **Rung 3 (optional).**
- `type WeightsInfo struct{ TotalBytes int64; Source WeightsSource }`; `WeightsInfo{}`
  (TotalBytes 0) => estimator uses the param-count fallback (**rung 4**, marks confidence
  `approximate`).

**Estimation**
- `func EstimateVRAM(cfg ModelConfig, w WeightsInfo, p Params) (Estimate, error)`
- `type Params struct{ Ctx int; Batch int; KVDtypeBytes int }` — Ctx required (>0); Batch
  defaults 1; KVDtypeBytes 2=fp16 (default) / 1=fp8 KV cache.
- `type Estimate struct{ WeightsMiB, KVCacheMiB, OverheadMiB, TotalMiB, BytesPerParam float64;
  Ctx, Batch int; IsGQA bool; GQARatio float64; Confidence Confidence; Notes []string }`
  (`TotalMiB` == required_vram = weights+kv+overhead; `Confidence` = `exact` | `approximate`).
- `func (e Estimate) FitsInVRAM(freeVRAMMiB float64) bool` — `TotalMiB <= freeVRAMMiB*0.92`.
- `func MaxContextThatFits(cfg, w, freeVRAMMiB float64, batch, kvDtypeBytes int) int` —
  closed-form solve of the (linear-in-ctx) KV budget; floored to a multiple of 256, capped
  at `max_position_embeddings`. Returns 0 if weights+overhead alone exceed usable VRAM.
- `func (e Estimate) GPUMemUtil(totalVRAMMiB float64) GPUMemUtilGuidance` — vLLM launch
  guidance (below).

## 2. What P2.3 passes in / gets out

P2.3 (verdict assembler) is the caller. It supplies:
- **`ModelConfig`** — from the model's `config.json`. For LOCAL models, read the file
  (inventory already records `local_path`); for the remote `POST /api/compat/preview` path,
  fetch `config.json` (+ `model.safetensors.index.json`) over HTTP without weights.
- **`WeightsInfo`** — try `ParseIndexTotalSize` first; else `WeightsFromShardSizes` from a
  file listing; else `SumLocalSafetensorsHeaders` for local files; else pass `WeightsInfo{}`
  to fall back to param-count (result flagged `approximate`).
- **Ctx candidates** — P2.3 should call `EstimateVRAM` at the *requested* ctx AND compute
  `MaxContextThatFits(free)` to produce the "fits at 8K, not 32K" degraded verdict
  (spec §4 "Context-length realism"). Requested-ctx estimate > free => try
  `MaxContextThatFits`; if that ≥ some floor (e.g. 2048) => `runs_degraded` with that ctx,
  else `wont_run`.
- **VRAM budget** — the **CURRENT free VRAM** from the live hardware probe
  (`hardware.Probe.Snapshot(ctx).GPUs[0].VRAMFreeMiB`, per wiring-p12.md §9), NOT total.
  For the vLLM gpu-memory-utilization figure, pass **total** VRAM (`GPUs[0].VRAMTotalMiB`).

Mapping to the verdict contract (spec §4 JSON): `estimated_vram_mb` = `Estimate.TotalMiB`;
`max_context_that_fits` = `MaxContextThatFits(...)`; `estimator` = `"localhub-safetensors"`;
`status` from `FitsInVRAM` / degraded logic; `reason` = a sentence built from `Notes`
(every Note is human-readable and already explains its assumption — join them for the reason).
`Confidence == approximate` SHOULD append a "estimate is approximate (no exact weights size)"
caveat to the verdict and, per the correctness bar, bias toward the more conservative status.

## 3. gpu_memory_utilization guidance (consumed by the P4 vLLM adapter)

`Estimate.GPUMemUtil(totalVRAMMiB)` returns:
```
type GPUMemUtilGuidance struct {
    RequiredMiB   float64 // = Estimate.TotalMiB
    TotalVRAMMiB  float64
    MinUtil       float64 // minimum --gpu-memory-utilization that holds RequiredMiB (= Required/Total)
    RecommendUtil float64 // MinUtil rounded up; >= 0.90 when the model fits under the default; capped at 0.97
    Fits          bool    // RequiredMiB <= RecommendUtil * TotalVRAMMiB
    Notes         []string
}
```
Semantics (spec §4(3)/§5): vLLM **pre-allocates** `--gpu-memory-utilization * TOTAL VRAM` and
FILLS that fraction with weights+activations+KV. So the estimator reports BOTH the required
VRAM (`RequiredMiB`) and the minimum utilization (`MinUtil`). **P4's vLLM adapter passes
`RecommendUtil` as the `--gpu-memory-utilization` launch flag.** `Fits == false` => the model
needs more than the safe ceiling (0.97) of the device even at max utilization → the verdict is
`wont_run` on vLLM (suggest smaller quant / tensor-parallel / a GGUF via llama.cpp).
`RecommendUtil` defaults up to vLLM's own 0.90 when the model fits comfortably, so vLLM sizes a
healthy KV cache rather than the bare minimum.

## 4. Formula summary (spec-digest §4, implemented exactly)

```
head_dim   = explicit config.head_dim if present, else hidden_size / num_attention_heads
n_kv_heads = config.num_key_value_heads if present, else num_attention_heads  (MHA)
KV_bytes   = 2(K,V) * n_layers * n_kv_heads * head_dim * ctx * batch * kv_dtype_bytes  (fp16=2, fp8=1)
weights    = index.total_size  ||  Σ shard sizes  ||  Σ local header sizes  ||  params * bytes_per_param
             bytes_per_param: fp16/bf16=2, fp8=1, 4-bit AWQ/GPTQ=0.5 (+10% scales, param-count path only)
overhead   = 2.0 GiB fixed  (vLLM CUDA ctx + activations + graph capture)
required   = weights + KV + overhead
fits       = required <= free_VRAM * 0.92        (safety_factor, PLAN res. 9 — margin on the SUPPLY side)
max_ctx    = floor_256( (free*0.92 - weights - overhead) / (KV_per_token * batch) ), capped at max_position_embeddings
min_util   = required / total_VRAM               (vLLM pre-allocates util * total)
```
KV sanity anchor (Llama-3.1-8B, 32L/8kv/128hd @8192 ctx fp16 batch1):
`2*32*8*128*8192*2 = 1,073,741,824 B = 1024 MiB = exactly 1 GiB`. Verified in
`TestLlama31_KVSanity1GiB`.

## 5. Assumptions where spec-digest §4 was SILENT (ORCHESTRATOR REVIEW — all in `constants.go`)

The doc gives ranges or omits a value; every choice below rounds toward **over-estimating
required VRAM** (conservative — a false `runs` that OOMs is a blocking bug, CLAUDE.md).

1. **vLLM fixed overhead = 2.0 GiB** (`vllmOverheadMiB`). Spec §4 says "overhead ≈ 1.5–2.0 GB
   fixed". Chose the **upper** bound (2.0 GiB) so the fit test never under-counts. If the
   orchestrator wants the estimate less conservative, lower to 1.5 GiB — single constant.
2. **safety_factor applied to the SUPPLY side** (`fits = required <= free*0.92`), not by
   inflating `required`. Value 0.92 is pinned by PLAN res. 9 / spec §4. Direction documented
   in `constants.go` and `FitsInVRAM`. (This is the estimator margin only; residency adds its
   own additive `SAFETY_MARGIN_MIB = 1024` later — PLAN res. 9 — NOT applied here.)
3. **vLLM gpu-memory-utilization ceiling = 0.97** (`vLLMMaxGPUMemUtil`). Spec gives the
   default (0.90) but no safe max; above ~0.97 vLLM commonly OOMs on CUDA-graph capture /
   fragmentation. Used as the cap for `RecommendUtil` and the `Fits` cutoff. **[RE-VERIFY]**
   against the pinned vLLM version before P4 relies on it.
4. **KV dtype default = fp16 (2 bytes)** unless the caller passes `KVDtypeBytes: 1`. vLLM's
   default KV dtype follows the model dtype (fp16/bf16 => 2). fp8 KV is opt-in
   (`--kv-cache-dtype`), exposed as a parameter, never defaulted.
5. **Param-count fallback model** (`params.go`) assumes a standard LLaMA-style decoder
   (SwiGLU MLP = 3 matrices, GQA-aware attn projections, tied/untied embeddings from
   `tie_word_embeddings`). Only used when NO authoritative weights size exists; it marks the
   estimate `approximate`. Non-SwiGLU MLPs are slightly over-counted (conservative). Prefer
   feeding a real `WeightsInfo` whenever the index/shards are available.
6. **"compressed-tensors" quantizer** mapped by bit-width: 8-bit => fp8 (1 B/param), else
   4-bit path (0.5 B/param). Unknown quantizers fold to unquantized (2 B/param — conservative).
7. **MaxContextThatFits floor = multiple of 256** (`ctxAlign`), per the task spec; keeps
   recommended ctx clean and slightly conservative.

## 6. Files created (all under `internal/compat/safetensors/`)

- `config.go` — `ModelConfig`, `ParseConfig`, GQA/MHA + head_dim resolution, quant detection.
- `weights.go` — `WeightsInfo` + the 4-rung size ladder (index / shards / local header / none).
- `params.go` — param-count fallback + `bytesPerParam`.
- `dtype.go` — `normalizeDtype`.
- `estimate.go` — `EstimateVRAM`, `Estimate`, `Params`, `FitsInVRAM`, `MaxContextThatFits`,
  `GPUMemUtil`, rounding helpers.
- `constants.go` — every constant + the silent-spec assumptions (§5 above), documented inline.
- `estimate_test.go`, `property_test.go` — unit + property tests (fixtures in `testdata/`).
- `testdata/` — real public configs: `llama31-8b-config.json` (+`-index.json`),
  `phi1_5-mha-config.json`, `llama2-7b-awq-config.json`, `gemma2-9b-headdim-config.json`.

## 7. Verification performed

- `go vet ./internal/compat/safetensors/...` — clean.
- `go test ./internal/compat/safetensors/...` — ok (all unit + property tests).
- `go build ./...` — clean (whole module). `go vet ./...` — clean.
- No new `go.mod` dependencies (stdlib only: encoding/binary, encoding/json, fmt, io, math,
  os, strings).

## 8. Dependencies / constants NOT owned here (forward notes)

- The residency additive `SAFETY_MARGIN_MIB` (PLAN res. 9) is NOT in this package — it belongs
  to `internal/residency`. This package owns only the estimator's multiplicative `safety_factor`.
- PLAN res. 9 asks for "one constants file, documented, in `internal/compat`". The
  compat-wide constants file (shared by gguf + safetensors + verdict) is a P2.3 concern; this
  package keeps its own `constants.go` (package-private) and documents the values here for that
  reconciliation. If P2.3 wants a single `internal/compat/constants.go`, promote `safetyFactor`
  and `vllmOverheadMiB` there and have this package reference them.
```
