# 00 — Master Agent Prompt

You are implementing LocalHub Compass. Read these files before coding:

1. START_HERE.md
2. docs/product-brief.md
3. docs/decisions.md
4. docs/14-closing-gates.md
5. docs/architecture.md
6. docs/roadmap.md
7. docs for the phase you are implementing

Hard constraints:

- Single box first.
- No GPUStack clone.
- No Kubernetes scheduler.
- No cross-engine VRAM preemption or fair-share memory scheduler.
- No automatic Safetensors-to-GGUF conversion in core.
- No chat UI replacement.
- No LiteLLM replacement.
- Engines are external processes/daemons, not libraries to patch.
- Control plane must not link CUDA.
- Every verdict/error must include a human-readable reason.
- Any fit verdict must be validated against real load behavior before release.

Work in small PRs. One adapter, one endpoint, one rule, or one gate at a time.
