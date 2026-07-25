# 23 — Phase 0 Field Validation Template

Complete this before writing product code.

## Machine

_Audited 2026-07-06 (P0.1)._

- OS: Windows 10 Home 10.0.19045.6466
- CPU: AMD Ryzen 5 2600 (6 cores / 12 threads)
- RAM: 15.9 GB
- GPU(s): NVIDIA GeForce RTX 4060 Ti (compute capability 8.9)
- VRAM: 16380 MiB total (at audit: 13293 MiB free / 2817 MiB used)
- Driver: 576.40 (driver-reported CUDA 12.9)
- CUDA/ROCm: CUDA toolkits installed: nvcc 13.2 (PATH), 12.5 (used by tq3 build scripts). Control plane links neither (ADR-005).
- Tailscale installed: **yes** — 1.96.3 (`C:\Program Files\Tailscale\tailscale.exe`)
- Disk: **K: 1.9 TB, only ~25 GB free; C: 229 GB, only ~6.1 GB free.** ⚠ Severe
  constraint: new-download budget revised down from 30 GB; validation reuses the
  ~60 GB of existing GGUFs in `K:\projects\llm_models\models\`.
- WSL2: present (default distro: docker-desktop); Docker Desktop 29.4.0.

## Tools tested

- Ollama version: 0.21.2 (client+app; daemon not running at audit time)
- llama.cpp / llama-server version: **two builds present**
  - winget: build 8833 (45cac7ca7), Clang 19.1.5 — backends: RPC, **Vulkan**, CPU (no CUDA backend)
  - local fork `llama.cpp-tq3` (github.com/turbo-tan/llama.cpp-tq3, commit 1cfa891): MSVC + **CUDA**, detects the 4060 Ti. Fork-only flags: `--sleep-idle-seconds`, KV type `tq3_0` (see research/lmwrapper-reuse-notes.md)
- vLLM version: not installed — no native Windows support; to be exercised once via Docker/WSL2 (P4.5)
- llama-swap version: not installed yet (trial scheduled in P0.2)
- gguf-parser-go version: not installed yet (install scheduled in P0.2)
- Open WebUI version: not installed
- Toolchain: Go 1.23.4; Node 24.11.1 (via nvm, not active on PATH — activate with `nvm use 24.11.1`); Docker 29.4.0

## Model attempts

_Filled 2026-07-06 (P0.2). Full detail + estimate-vs-actual deltas + friction list in
`research/phase-0-friction-log.md`. Estimator = gguf-parser v0.24.1 NONUMA VRAM, full
offload, FA on. Baseline free VRAM 13.3 GB (desktop already using ~2.8 GB); usable under
1.5 GB margin ≈ 11.75 GB._

| # | Model | Format | Source | Target engine | Estimated fit? | Actual result | Friction | Keep/Drop insight |
|---:|---|---|---|---|---|---|---|---|
| 1 | GLM-OCR | GGUF Q8_0 | local | llama.cpp fork | 1582 MiB @4K | **LOADED** HEALTHY, 1 completion; VRAM Δ~1105 MiB, freed | estimate +43% over; mmproj excluded | KEEP |
| 2 | Qwen3.6-27B-TQ3_4S | GGUF TQ3_4S | local | llama.cpp fork | parser **fails** (GGMLType46); ~13.3 GB | not loaded (safeguard: free<14GB, >usable) | estimator blind to TQ3; fit set by real free VRAM | KEEP flagship |
| 3 | Qwen3.5-27B-TQ3_1S | GGUF TQ3_1S | local | llama.cpp fork | parser **fails** (TQ3) | not loaded (estimate-only) | TQ3 opaque; bat-script evidence it runs | KEEP |
| 4 | Qwopus3.5-27B-v3-TQ3_4S | GGUF TQ3_4S +mmproj | local | llama.cpp fork | parser **fails** (TQ3) | not loaded (estimate-only) | TQ3 gap + mmproj unaccounted | KEEP vision |
| 5 | Qwen3.6-35B-A3B-TQ3_4S | GGUF TQ3_4S MoE | local | llama.cpp fork | parser **fails** (TQ3) | not loaded (estimate-only) | MoE active≪total; naive size rule false-negatives | KEEP (counter-intuitive fit) |
| 6 | Qwen3-VL-32B-Instruct | GGUF Q2_K +mmproj | local | llama.cpp fork | **12989 MiB** @8K (parses) | not loaded (>usable @ baseline) | standard quant parses → gap is TQ3-specific | KEEP boundary |
| 7 | Qwen3-VL-Embedding-8B | GGUF Q6_K | local | llama.cpp fork | 7373/7957 MiB @4K/8K | not loaded (fits; load budget) | embed treated as generative KV | KEEP |
| 8 | nomic-embed-text-v2-moe | GGUF Q4_K_M | Ollama+GGUF | Ollama | 579 MiB @4K | **LOADED via Ollama** warm+unload; ps=911, NVML Δ~700 | 3 tools, 3 numbers, no source of truth | KEEP |
| 9 | Meta-Llama-3.1-8B-Instruct | GGUF Q4_K_M (GQA) | remote HF | llama.cpp/Ollama | **6218/9338 MiB** @8K/32K (remote, no dl) | not loaded (remote estimate-only; fits) | no cross-ref to live free VRAM/margin | KEEP GQA |
| 10 | Llama-3.3-70B-Instruct | GGUF Q4_K_M | remote HF | none viable | **43354 MiB** @8K (remote) | not loaded — **won't-fit (correct)** | number given, no verdict/reason | KEEP won't-fit ref |
| 11 | Llama-3.2-1B-Instruct | GGUF Q4_K_M | remote HF | llama.cpp/Ollama | **1798/2614 MiB** @8K/32K (remote) | not loaded (remote estimate-only; trivial fit) | even easy case needs manual MiB-vs-budget | KEEP baseline |

## Decision

**`GO_STANDALONE`** — recorded 2026-07-06 (P0.3).

Rationale:

```text
Decision authority: project owner directive (2026-07-06) — "I want this whole project
done" — stands as the GO decision per PLAN.md decision 2. The field trial (P0.2)
independently supports it: no existing tool provides the single-box bundle.

Concrete evidence from the friction log (research/phase-0-friction-log.md):
- gguf-parser gives raw numbers with no verdict/reason/margin/max-ctx, cannot see
  actual free VRAM (real ceiling was 13.3 GB, not 16), and fails entirely on the
  TQ3 quants this box's four flagship models use.
- Ollama, gguf-parser, and NVML report three disagreeing memory numbers with no
  source of truth; mmproj VRAM is invisible; MoE active-vs-total confuses fit logic.
- No unified inventory exists across Ollama models + loose GGUFs + HF dirs; no
  format→engine routing; vLLM is impossible natively on Windows; no phone access.
- llama-swap swaps processes well (proven: swap-on-request, /running, unload, TTL)
  but has no VRAM-aware admission, no estimates, no priority, no reasons — the
  safety/verdict layer LocalHub is for does not exist in it.
CONTRIBUTE_UPSTREAM was considered: llama-swap would only cover the llama.cpp
executor slice; the wedge (verdicts + admission + manifests + multi-engine + phone)
spans tools that don't share a roadmap. STOP fails: the pain reproduced immediately
on a real workload.
```
