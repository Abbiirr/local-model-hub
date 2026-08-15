# Product Brief

## The one-sentence product

A single-machine control plane that answers *"what can I run, on what, how, and is it
still running?"* for local LLMs across llama.cpp and vLLM — and lets you drive
it from your phone, including through OpenAI-compatible clients and an experimental
Anthropic Messages adapter for Claude Code.

## Target user

The person who has **one workstation with one or two consumer/prosumer GPUs** (e.g. RTX
4060 Ti 16 GB, 4090, 3090, or a Mac) and runs several local models across more than one
engine. They:

- pull models from Hugging Face and keep GGUFs lying around,
- want to know *before* downloading 40 GB whether a model fits,
- get bitten by "loaded two models, second one OOM'd,"
- want to check GPU/queue status from their phone without SSH,
- care (a little) about reproducibility and not running pickle blobs blindly.

This is **not** an enterprise fleet operator. If you have Kubernetes and 40 GPUs, use
GPUStack or KubeAI. That is a deliberate scoping decision, not a gap to fill later.

## The wedge (be honest about it)

The differentiator is **not** any single novel capability. VRAM estimation exists
(`gguf-parser-go`). Process swapping exists (llama-swap). Chat UIs exist (Open WebUI).
Gateways exist (LiteLLM). Cluster management exists (GPUStack).

The wedge is the **bundle at single-box scale**:

> pre-flight compatibility across *all three engines at once* + supply-chain/repro
> manifests + coarse residency + phone ops — packaged so a solo GPU owner installs it in
> minutes and it Just Works on one box.

Each competitor owns one slice; none owns this combination for the single-box user. That
is thin but real. If during Phase 0 you find a tool that already nails this bundle,
**stop and contribute to it instead** — that outcome is a success, not a failure.

## Competitor delta

| Tool | Multi-engine | Pre-flight compat | Single-box focus | Supply-chain/repro | Coarse residency | Phone ops | Verdict |
|---|---|---|---|---|---|---|---|
| **GPUStack** | ✅ (vLLM, SGLang, MindIE, VoxBox) | ✅ (uses gguf-parser) | ❌ cluster-first | partial | ✅ scheduler | via web | Closest. Heavy; cluster-oriented; k8s-adjacent. |
| **Xinference** | ✅ | partial | partial | ❌ | ✅ | via web | Multi-backend serving; not compat/supply-chain focused. |
| **Ollama** | ❌ (own format) | ❌ | ✅ | ❌ | coarse (keep_alive) | ❌ | Great UX, single engine, hides everything. |
| **llama-swap** | ✅ (any OpenAI/Anthropic server) | ❌ | ✅ | ❌ | ✅ (process swap) | web UI | Excellent swap layer. No inventory/compat/supply-chain. |
| **LiteLLM** | proxy-level | ❌ | n/a | ❌ | ❌ | ❌ | Gateway/routing. Different layer. |
| **Open WebUI** | via APIs | ❌ | ✅ | ❌ | ❌ | ✅ | Chat UI. Different layer. |
| **LocalAI** | ✅ | partial | ✅ | ❌ | ✅ | via web | Multi-backend OpenAI server. Not compat/supply-chain UX. |
| **LM Studio** | ❌ (own) | partial (fit hints) | ✅ | ❌ | ✅ | ❌ | Closed-source desktop. Single-box, single-ish engine. |
| **LocalHub Compass** | ✅ | ✅ **core UX** | ✅ **first** | ✅ **core UX** | ✅ coarse | ✅ | The bundle. |

Sources for the capability claims are in [`decisions.md`](decisions.md) and the
validation report; re-verify quarterly, this space moves monthly.

> Validation note, 2026-07-06: current GPUStack built-in backend docs list vLLM, SGLang, MindIE, and VoxBox. Older notes mentioning llama-box should be treated as historical/stale until re-verified.


## Non-goals

These are **not** MVP, and several are permanent non-goals. Building them means you've
drifted back into cloning something that already exists.

```
❌ GPUStack clone / multi-node cluster manager
❌ Kubernetes operator or scheduler
❌ Live cross-engine VRAM preemption / weighted-fair GPU memory scheduling
   (see ADR-001 — this is not buildable on top of independent engine processes today)
❌ Automatic Safetensors → GGUF conversion pipeline in core
   (see ADR-004 — advanced/manual path only)
❌ A chat UI to replace Open WebUI
❌ An AI gateway/router to replace LiteLLM
   (the narrow experimental Anthropic Messages protocol adapter is the exception; budgets,
   keys, multi-provider routing/fallback, rate limits, and caching remain out of scope)
❌ Training / fine-tuning orchestration
❌ Multi-tenant SaaS auth/billing
```

## Success criteria for MVP

1. Point it at a fresh machine with llama.cpp/vLLM + local GGUF or Safetensors artifacts; it produces a correct
   inventory and a correct *runs / won't-run* verdict per model with no manual config.
2. It never OOMs the GPU by loading a model that doesn't fit — admission control refuses
   or evicts first.
3. It never kills an in-flight generation to make room (by default).
4. You can open it on your phone over Tailscale and see live GPU + running models.
5. Every downloaded model has a manifest that lets you reproduce the exact artifact.
6. Claude Code can complete a streamed text-and-client-tool loop through the experimental
   Anthropic Messages adapter against a supported local OpenAI-compatible engine.

If it does those six things reliably on one box, it is worth shipping — regardless of
how many bigger tools exist.

The Anthropic surface is **wire compatibility, not model equivalence**. A local model
served through `/v1/messages` is not Claude, is not provided or supported by Anthropic,
and may differ materially in instruction following, tool use, safety behavior, context,
and output quality.
