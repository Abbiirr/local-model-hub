# 18 — Source Validation Matrix

_Last checked: 2026-07-07_

Re-check this file before each release and anytime an adapter/estimator behavior changes.

| Claim | Primary source to re-check | Current plan decision |
|---|---|---|
| GPUStack current built-in backends | GPUStack built-in inference backend docs | Product brief says GPUStack is closest competitor, not target to clone. Current row: vLLM/SGLang/MindIE/VoxBox. |
| GPUStack compatibility checks and VRAM estimates | GPUStack model catalog / compatibility docs | Do not claim LocalHub invented compatibility checks; wedge is single-box bundle + manifests. |
| Ollama residency controls | Ollama FAQ + API docs | Use `keep_alive`, `/api/ps`, `/api/tags`; treat as coarse daemon controls. |
| Ollama concurrent model behavior | Ollama FAQ | New GPU models must fit in VRAM for concurrent loads. |
| Ollama OpenAI-compat surface | https://docs.ollama.com/api/openai-compatibility | **Verified 2026-07-06:** Ollama serves `/v1/chat/completions`, `/v1/completions`, `/v1/models`, `/v1/embeddings` at base `http://127.0.0.1:11434/v1`, so the P6.1 proxy relays uniformly across all three engines with no per-engine translation. |
| vLLM CUDA requirement | vLLM installation GPU docs | Gate NVIDIA CUDA vLLM at compute capability 7.5+ unless exact version says otherwise. **Verified 2026-07-06:** GPU installs require Linux + Python 3.10–3.13; Windows is not natively supported (Docker/WSL2/attach only). |
| vLLM launch + health contract | https://docs.vllm.ai/en/stable/serving/online_serving/ | **Verified 2026-07-06** (internal/engines/vllm/command.go RE-VERIFY note): docker run + `GET /health` + `GET /v1/models`; `--model <repo>` passed after the image name (version-sensitive — pin the image, re-verify on upgrade). |
| vLLM sleep mode | vLLM sleep mode docs | Advanced opt-in only; dev endpoints not public; not a cross-engine scheduler primitive. **Verified 2026-07-06:** gated behind `VLLM_SERVER_DEV_MODE=1 --enable-sleep-mode`; the proxy never forwards `/sleep`,`/wake_up`,`/collective_rpc`,`/reset_prefix_cache`. |
| vLLM GGUF | vLLM GGUF docs | Do not route GGUF to vLLM in MVP; use llama.cpp/Ollama. **Verified 2026-07-06:** GGUF remains experimental/under-optimized and requires the out-of-tree `vllm-gguf-plugin` (ADR-004). |
| llama.cpp server routes | llama.cpp server README | `llama-server` is suitable as a supervised OpenAI-compatible local process. **Verified 2026-07-06:** OpenAI-compatible chat/completions/embeddings + monitoring routes confirmed. Fork (turbo-tan/llama.cpp-tq3 @1cfa891) is fork-only for **tq3_0 KV cache + TQ3 weight kernels + CUDA backend**; `--fit`, `--sleep-idle-seconds`, `--models-dir`, `--reasoning-budget` are now upstream too (winget b8833). **The winget build has NO CUDA (Vulkan/CPU only)** — route GPU work to the fork. |
| gguf-parser-go estimator | https://github.com/gpustack/gguf-parser-go | **Verified 2026-07-06:** **v0.24.1** reads local + remote HF GGUF metadata by HTTP range request (no weight download); **cannot parse TQ3** (`GGMLType(46)` unsupported, even metadata) → LocalHub uses a labelled conservative fallback heuristic + measured-peak calibration. |
| llama-swap capabilities | llama-swap README | Try driving llama-swap before implementing a custom residency manager. **Trialled 2026-07-06 (v235):** swap / `/running` / unload / TTL all work; NO VRAM-aware admission, estimate, priority, or reason → ADR-003 resolved to build custom residency above it. |
| Tailscale Serve | Tailscale Serve docs | Preferred phone ops path: LocalHub on localhost, Serve to tailnet. **Verified 2026-07-06:** Serve proxies localhost into the tailnet, applies ACLs, injects identity headers, and strips spoofed ones (trust identity headers only behind a localhost bind). |
| Safetensors safety positioning | Hugging Face safetensors docs | Prefer safetensors; warn on pickle/trust_remote_code; strict mode optional. |

_2026-07-07 note:_ the dated "Verified" annotations above are sourced from the primary-source
re-checks recorded in `docs/decisions.md` §"Source revalidation log — 2026-07-06" and
`docs/full-project-review-2026-07-06.md` §"Web / Primary Sources". They record verifications
that actually happened on those dates; no new verification is claimed beyond them.
