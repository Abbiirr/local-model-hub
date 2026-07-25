# LocalHub Compass — Start-to-End Handoff

This is the concise control document. The full project package is in `localhub-compass-project-handoff.zip`.

## Validated direction

Build the narrowed single-box LocalHub Compass, not the original broad LocalHub. The product is a local model operations control plane for one workstation with one or two GPUs.

## What to build

1. Inventory local models across Ollama, GGUF folders, and HF/safetensors directories.
2. Predict compatibility with exact hardware and recommend engine/flags.
3. Download/import models with immutable manifests and warnings.
4. Start/stop models through Ollama, llama.cpp, and vLLM.
5. Safely manage coarse residency with whole-model load/unload.
6. Show GPU/model/queue status on phone through Tailscale Serve.
7. Optionally add a thin OpenAI-compatible proxy after residency works.

## What not to build

- GPUStack clone.
- Kubernetes or multi-node scheduler.
- Cross-engine VRAM preemption / fair-share scheduler.
- Automatic Safetensors-to-GGUF conversion pipeline.
- Chat UI replacement.
- LiteLLM replacement.
- Training/fine-tuning platform.
- Multi-tenant SaaS auth/billing.

## Close gates

Use `docs/14-closing-gates.md`. Non-waivable blockers:

- No false-positive `runs` verdict that OOMs.
- No active stream killed by default.
- No model artifact without reproducible manifest.
- No public admin exposure by default.
- No scheduler/conversion/gateway scope creep.

## First action

Run Phase 0 and fill `docs/23-phase-0-field-validation.md` before writing product code.
