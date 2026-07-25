# AGENTS.md — Agent Working Instructions

Instructions for AI coding agents (Claude Code and similar) working in this repo. Read
this fully before writing code.

## Discovery first — do not code from assumptions

1. **Read before writing.** In order: this file → `docs/product-brief.md` (esp. non-goals)
   → `docs/decisions.md` (ADRs) → the specific doc for the area you're touching.
2. **The docs are the source of truth for intent; the code is the source of truth for
   current state.** When they disagree, surface the conflict — do not silently pick one.
3. **Never invent engine behavior.** Claims about what vLLM/Ollama/llama.cpp can do are
   time-sensitive and were verified at authoring time. Before relying on one, re-check the
   engine's current docs/GitHub. If you can't verify, say so and flag it — don't guess.

## Hard constraints (these come from ADRs — violating them wastes work)

- **No cross-engine VRAM preemption / fair-share scheduler.** Residency is coarse
  load/unload of whole models, always re-measuring free VRAM via NVML. [ADR-001]
- **Don't reinvent the estimator or the gateway.** Wrap `gguf-parser-go` (GGUF) and keep
  the OpenAI proxy a thin passthrough; LiteLLM owns gateway features. [ADR-002]
- **No auto Safetensors→GGUF conversion in core.** Route by format; conversion is manual/
  advanced only. Pickle → warn, don't hard-block by default. [ADR-004]
- **Engines are external supervised processes.** Never fork/patch them or link their
  memory internals. The control plane must not link CUDA. [ADR-005]
- **Single box first.** No cluster/k8s assumptions in the data model, API, or scheduler.
  [ADR-007]

## Scope test before adding anything

Does the change serve: *"which local models run on this one box, on what engine, at what
flags, still running, over my phone"*? If it edges into chat UI, multi-provider gateway,
k8s, or training — **stop and ask.** Those are permanent non-goals, not future phases.

## Correctness bars

- Any code path that decides a model "fits" must be validated against real load behavior.
  A false `runs` that OOMs is a correctness bug. Prefer conservative estimates + margins.
- `stop`/`evict` must verify VRAM is actually freed (NVML delta), not trust an API 200.
- Never let the residency manager kill an instance with active in-flight requests by
  default (`DRAINING`, not killed).
- Every user-facing verdict/error must carry a human-readable `reason`.

## Build order

Follow `docs/roadmap.md` phases. Each phase has a Definition of Done; don't start a phase
before the prior DoD is met. Phase 0 (validate with existing tools) is not optional — its
output may legitimately be "contribute upstream instead," and that's a success.

## When unsure

Ask, or leave a clearly marked `TODO(agent): needs human decision — <why>` rather than
guessing on: the language decision (ADR-006), the llama-swap vs custom-residency call
(ADR-003), anything that would touch a non-goal, or any engine-capability claim you
couldn't verify against a current primary source.

## Working style

- Small, single-purpose changes. One adapter, one rule, one endpoint per unit of work.
- Match existing code conventions once code exists; until then, follow the stack chosen in
  ADR-006.
- Cite the primary source (engine doc / GitHub issue) in commit messages when a change
  depends on external engine behavior — it makes the next re-verification cheap.
