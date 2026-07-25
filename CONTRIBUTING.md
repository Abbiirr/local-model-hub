# Contributing to LocalHub Compass

Thanks for helping. This is a focused tool with a deliberately **narrow scope** — the
fastest way to get a PR merged is to work *with* that scope, not against it.

## Read these first

1. [`docs/product-brief.md`](docs/product-brief.md) — what this is and, importantly, the
   **non-goals**. PRs that drift toward "let's also be a chat UI / gateway / k8s
   scheduler" will be declined, no matter how good.
2. [`docs/decisions.md`](docs/decisions.md) — the ADRs. If your change contradicts an
   accepted ADR, open an issue proposing a superseding ADR *before* coding.

## Scope guardrails (the quick test)

Before opening a PR, ask: does this make LocalHub better at **"which local models run on
my box, on what engine, at what flags, and are they still running — on one machine, over
my phone"**? If yes, great. If it's adding a capability that GPUStack / LiteLLM / Open
WebUI / Ollama already own, it's probably out of scope — file an issue and let's discuss.

## Dev setup

_(Fill in once the stack is chosen per [ADR-006](docs/decisions.md#adr-006). Skeleton:)_

```bash
# clone, then:
# Go control plane:      go build ./... && ./localhub --config dev.yaml
# or Python control plane: uv sync && uv run localhub --config dev.yaml
# frontend:              cd web && pnpm i && pnpm dev
```

You need at least one engine installed to do anything useful. Easiest: Ollama + a couple
of small GGUFs. `gguf-parser` (or the Go lib) is required for compatibility work.

## Adding a new engine adapter

The most valuable kind of contribution. Implement the contract in
[`docs/engine-adapters.md`](docs/engine-adapters.md):

```
detect / list_models / list_running / start / stop / health / supports_sleep / openai_base_url
```

Then answer these in your PR description, because they determine residency behavior:

- **Ownership:** does LocalHub spawn the process (like llama.cpp/vLLM) or is it a shared
  daemon (like Ollama)?
- **Unload:** how is VRAM actually freed, and how do you *verify* it's freed (NVML delta,
  not just an API 200)?
- **Ports:** one shared port or one per model?
- **Cold start:** how slow, and does the UI need a `STARTING` affordance?

Include the adapter test checklist results (bottom of `engine-adapters.md`).

## Compatibility rules

Estimation changes must come with **measured error** on a spread of real models (GGUF +
safetensors, GQA + MHA, fits + doesn't-fit). A rule that says `runs` for something that
OOMs is a correctness bug, not a tuning nit. Never widen a `runs` verdict without data.

## Style

- Small, single-purpose PRs. One adapter, one rule fix, one endpoint.
- Every user-facing verdict/error carries a human-readable `reason`. No black boxes.
- Time-sensitive facts about engines change monthly — cite a primary source (engine docs,
  GitHub issue) in the PR when your change depends on engine behavior.

## What we won't merge

- Cross-engine VRAM preemption / fair-share scheduling ([ADR-001](docs/decisions.md#adr-001)).
- Auto Safetensors→GGUF conversion in core ([ADR-004](docs/decisions.md#adr-004)).
- A bundled chat UI or a LiteLLM-style gateway (non-goals).
- Anything that requires the control plane to link CUDA ([ADR-005](docs/decisions.md#adr-005)).
