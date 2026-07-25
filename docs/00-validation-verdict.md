# 00 — Validation Verdict and Plan Corrections

_Last validated: 2026-07-06_

## Verdict

The uploaded research is directionally correct and should be accepted with two corrections:

1. **Keep the narrowed product scope.** Build LocalHub Compass as a single-box compatibility, inventory, manifest, coarse-residency, and phone-ops control plane. Do not build a GPUStack clone, Kubernetes scheduler, chat UI, LiteLLM-style gateway, training system, or multi-tenant SaaS platform.
2. **Correct the GPUStack competitor row.** Current GPUStack built-in backend docs list **vLLM, SGLang, MindIE, and VoxBox**. Older references to `llama-box` as a current built-in backend must be treated as stale until re-verified.

## Why the narrowed plan still holds

LocalHub Compass has a narrow but valid wedge: it bundles pre-flight compatibility across Ollama, llama.cpp, and vLLM; model provenance/reproducibility manifests; conservative whole-model load/unload; and phone-accessible GPU/queue status. The uploaded README and product brief already frame it this way, and the roadmap deliberately starts with Phase 0 validation before writing product code.

## Source-backed corrections

| Area | Current source-backed state | Plan impact |
|---|---|---|
| GPUStack | GPUStack already does compatibility checks, VRAM estimation, model deployment, and multiple built-in backends. Current built-ins are vLLM, SGLang, MindIE, VoxBox. | LocalHub must stay single-box and not compete as a cluster manager. |
| Ollama | `keep_alive` can keep/unload a model; `/api/ps` exposes loaded/running models; concurrent loads require enough VRAM. | Treat Ollama as a shared daemon with coarse controls only. |
| llama.cpp | `llama-server` exposes OpenAI-compatible routes and is process-owned from LocalHub’s perspective. | LocalHub can safely supervise one process per llama.cpp model instance. |
| vLLM | NVIDIA CUDA requirement is compute capability 7.5+ in current docs; sleep mode server endpoints require dev mode; GGUF support is experimental and plugin-based. | Keep vLLM as the safetensors/AWQ/GPTQ/FP8 path. Do not route GGUF to vLLM in MVP. |
| Tailscale | Serve proxies local services to the tailnet, applies ACLs, and can pass identity headers. | Bind LocalHub to localhost; use Tailscale Serve for phone access. |
| Safetensors | Hugging Face positions safetensors as safer than pickle. | Prefer safetensors; warn on pickle/trust_remote_code; strict mode optional. |

## Go / no-go rule

At the end of Phase 0, the team must make one explicit decision:

- **GO standalone:** existing tools cannot satisfy the single-box bundle without unacceptable manual work.
- **CONTRIBUTE upstream:** llama-swap + gguf-parser + Open WebUI/GPUStack already cover the real need well enough.
- **STOP:** the wedge is not distinct enough or the validation workload cannot reproduce the pain.

A “contribute upstream” decision is not failure. It is a valid close gate.
