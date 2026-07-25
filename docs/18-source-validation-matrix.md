# 18 — Source Validation Matrix

_Last checked: 2026-07-06_

Re-check this file before each release and anytime an adapter/estimator behavior changes.

| Claim | Primary source to re-check | Current plan decision |
|---|---|---|
| GPUStack current built-in backends | GPUStack built-in inference backend docs | Product brief says GPUStack is closest competitor, not target to clone. Current row: vLLM/SGLang/MindIE/VoxBox. |
| GPUStack compatibility checks and VRAM estimates | GPUStack model catalog / compatibility docs | Do not claim LocalHub invented compatibility checks; wedge is single-box bundle + manifests. |
| Ollama residency controls | Ollama FAQ + API docs | Use `keep_alive`, `/api/ps`, `/api/tags`; treat as coarse daemon controls. |
| Ollama concurrent model behavior | Ollama FAQ | New GPU models must fit in VRAM for concurrent loads. |
| vLLM CUDA requirement | vLLM installation GPU docs | Gate NVIDIA CUDA vLLM at compute capability 7.5+ unless exact version says otherwise. |
| vLLM sleep mode | vLLM sleep mode docs | Advanced opt-in only; dev endpoints not public; not a cross-engine scheduler primitive. |
| vLLM GGUF | vLLM GGUF docs | Do not route GGUF to vLLM in MVP; use llama.cpp/Ollama. |
| llama.cpp server routes | llama.cpp server README | `llama-server` is suitable as a supervised OpenAI-compatible local process. |
| llama-swap capabilities | llama-swap README | Try driving llama-swap before implementing a custom residency manager. |
| Tailscale Serve | Tailscale Serve docs | Preferred phone ops path: LocalHub on localhost, Serve to tailnet. |
| Safetensors safety positioning | Hugging Face safetensors docs | Prefer safetensors; warn on pickle/trust_remote_code; strict mode optional. |
