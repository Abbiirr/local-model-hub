# Compatibility Engine

This is the product's core differentiator. For every (model, hardware) pair it answers:

- Can this run on my hardware? (`runs` / `runs_degraded` / `wont_run`)
- Which engine should run it?
- How much VRAM will it need?
- Fully on GPU, or CPU offload required?
- What context length is realistic?
- Which exact launch flags?
- What warning should the user see?

Output is a structured verdict (contract below). The UI renders it; the residency manager
consumes the VRAM estimate.

## Golden rule: reuse estimation, do not reinvent it

For **GGUF**, use **`gguf-parser-go`** (Apache-2.0, github.com/gpustack/gguf-parser-go).
It is the same estimator GPUStack uses. Critically, it reads GGUF metadata via **HTTP
range requests without downloading the whole model**, so you can produce a verdict
*before* a 40 GB download. It accounts for ctx-size, gpu-layers, flash-attention, KV
cache type, mmap, and multi-device tensor-split, and emits JSON.

- **Go control plane:** import it as a library.
- **Python control plane:** shell out to the `gguf-parser` binary with `--json`.

Do **not** write your own GGUF KV/weight estimator. You will get quant block sizes and
per-arch layer layouts subtly wrong, and this library already tracks upstream llama.cpp.

Example (pre-download, remote):

```bash
gguf-parser \
  --hf-repo Qwen/Qwen2.5-Coder-7B-Instruct-GGUF \
  --hf-file qwen2.5-coder-7b-instruct-q4_k_m.gguf \
  --ctx-size 8192 --flash-attention \
  --estimate --json
```

Parse `VRAM.nonuma` (bytes) as the estimate; compare against **current free VRAM** from
the hardware probe, not total.

## The gap gguf-parser-go does NOT cover: safetensors / vLLM path

`gguf-parser-go` is GGUF-only. For safetensors models on the vLLM path you must estimate
yourself. Three terms:

### 1. Weights

Prefer the authoritative size, in this order:

1. `model.safetensors.index.json` → `metadata.total_size` (bytes, exact).
2. Sum of safetensors shard sizes from the HF API file listing.
3. Fallback: `params × bytes_per_param` where bytes_per_param = 2 (fp16/bf16), 1 (fp8),
   ~0.5 (4-bit AWQ/GPTQ, plus small scales/zeros overhead — add ~10%).

### 2. KV cache (get this right — the common formula is wrong)

> **Independent K/V dtypes + the calibration key (2026-07-25).** `-ctk` and `-ctv` are sized
> **independently**: `compat.LaunchProfile.KVTypeK`/`KVTypeV` flow into
> `gguf.Options.KVCacheTypeK`/`KVCacheTypeV`, each falling back to the shared `KVCacheType`, then
> f16. The validated Qwen3.6-27B MTP profile is `-ctk q8_0 -ctv q4_0`.
>
> **Always populate the legacy `KVCacheType` too.** The observed-peak calibration store is indexed
> by a *single* `kv_type` column, and `gguf.CanonicalKVKey(k, v)` renders the pair into it
> (symmetric → `"f16"`/`"q4_0"`, preserving every pre-existing row; asymmetric → `"q8_0/q4_0"`).
> Leaving it empty makes the key `""`, so every recorded measurement misses and the verdict
> silently degrades from `exact` to `fallback` confidence — a real regression once shipped, now
> pinned by `TestVerdictUsesObservedPeak` / `TestVerdictTightensAfterRecording`.
> See KNOWN-LIMITATIONS #20.

Most online calculators use `bytes_per_value × hidden_size × n_layers × ctx`. That is
**wrong for modern models**, which use Grouped-Query Attention (GQA): the KV cache scales
with `n_kv_heads`, not `n_attention_heads`. Using `hidden_size` over-estimates KV by the
GQA ratio (often 4–8×).

Correct, GQA-aware, per-sequence:

```
head_dim   = hidden_size / n_attention_heads
kv_bytes   = 2 (K and V)
           × n_layers
           × n_kv_heads          -- from config: num_key_value_heads (NOT num_attention_heads)
           × head_dim
           × seq_len             -- context tokens you plan to serve
           × kv_dtype_bytes      -- 2 for fp16 KV, 1 for fp8 KV cache
           × batch               -- concurrent sequences (start with a modest default, e.g. 4–8)
```

Read `num_key_value_heads`, `num_attention_heads`, `hidden_size`, `num_hidden_layers`
from the model's `config.json` (fetchable from HF without the weights). If
`num_key_value_heads` is absent, the model is MHA → `n_kv_heads = n_attention_heads`.

### 3. vLLM runtime overhead (bigger than llama.cpp's)

vLLM reserves more than the naive weights+KV: CUDA context, activation buffers, and
**CUDA graph capture**. Two ways to model it:

- **Simple/conservative (MVP):** `overhead ≈ 1.5–2.0 GB` fixed, plus respect vLLM's
  `--gpu-memory-utilization` (default 0.9): vLLM will try to *fill* that fraction with KV
  cache. So the real fit question for vLLM is: **do the weights fit inside
  `gpu_memory_utilization × VRAM` with enough left over for the KV cache needed to serve
  `--max-model-len`?**
- If weights alone exceed `gpu_memory_utilization × free_VRAM` → `wont_run` on this GPU
  without tensor-parallel / smaller quant / CPU offload (which vLLM does not do well).

### Total (non-GGUF)

```
est_vram = weights + kv_bytes(at target ctx & batch) + overhead
fits     = est_vram <= free_vram_now * safety_factor      -- safety_factor ~0.92
```

## Rule engine

Start **rule-based**, not ML. Rules are auditable and users trust a stated reason.

### Hardware capability gates (checked first)

| Engine | Hard requirement | Source |
|---|---|---|
| vLLM | NVIDIA CUDA compute capability ≥ 7.5; Linux; Python 3.10–3.13; recent CUDA/ROCm as applicable | vLLM docs, rechecked 2026-07-06 |
| Ollama | broad; GPU or CPU; modern drivers | Ollama docs |
| llama.cpp | broadest (CPU, CUDA, ROCm, Metal, Vulkan) | llama.cpp README |

If a model is only distributed as safetensors and the only viable GPU engine (vLLM) is
gated out by compute capability, the verdict is `runs_degraded` via llama.cpp **iff** a
GGUF exists, else `wont_run` on GPU (CPU offload possible but slow → say so).

### Engine recommendation logic

```
format == gguf         → llamacpp (or ollama if the model is in Ollama's registry)
format == safetensors  → vllm  (if hardware gate passes)
                       → else: suggest a GGUF equivalent for llamacpp, if one is known
awq / gptq / fp8       → vllm
ollama-registry ref    → ollama
```

### Context-length realism

Estimate at the user's requested ctx AND at the model's max. If `runs` only at reduced
ctx, return `runs_degraded` with the max ctx that fits. Surface this — "fits at 8K, not
at 32K on your 16 GB" is exactly the insight users lack today.

## Verdict contract

```json
{
  "model": "qwen2.5-coder-7b-q4_k_m.gguf",
  "hardware": { "gpu": "RTX 4060 Ti", "vram_total_mb": 16380, "vram_free_mb": 15200 },
  "status": "runs",                       // runs | runs_degraded | wont_run | unknown
  "engine_recommendation": "llamacpp",
  "estimated_vram_mb": 6200,
  "estimator": "gguf-parser-go@vX.Y.Z",   // or "localhub-safetensors" for the vLLM path
  "gpu_only": true,
  "cpu_offload_layers": 0,
  "max_context_that_fits": 16384,
  "requested_context": 8192,
  "run_flags": { "ctx_size": 8192, "n_gpu_layers": 99, "flash_attn": true },
  "warnings": [],
  "reason": "GGUF Q4_K_M weights ~4.4GB + KV ~1.6GB at 8K fits in 15.2GB free with headroom"
}
```

`reason` is not optional. A verdict without a human-readable reason is a black box, and
the whole point is to *not* be a black box.

## What to deliberately NOT do in v1

- No ML-based estimator. Rules first; only consider learned correction if rule error is
  demonstrably large across many models.
- No promise of exact VRAM. State it's an estimate; real usage varies by backend/build.
  `gguf-parser-go` itself says this. Show estimate + a margin, not a false-precision number.
- No throughput/TPS promises in v1. `gguf-parser-go` can estimate max TPS given device
  FLOPS/bandwidth; treat that as a v2 "nice to have," clearly labeled as rough.


## Source revalidation note — 2026-07-06

Current vLLM installation docs list NVIDIA CUDA GPU compute capability 7.5 or higher. Do not loosen this gate without checking the exact vLLM version and build target.
