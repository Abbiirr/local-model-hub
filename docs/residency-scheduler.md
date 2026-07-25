# Residency Manager (Coarse Scheduler)

## What this is and is NOT

**IS:** process-level admission control and eviction. "Before I load model X, is there
room? If not, unload an idle/expired/low-priority model first. Never kill an active
generation. Never touch a pinned model."

**IS NOT:** a cross-engine VRAM scheduler, weighted-fair GPU memory allocator, or
preemption system. That was in the original LocalHub plan and it is **not buildable** on
top of three independent engine processes today — see [ADR-001](decisions.md#adr-001) for
the full evidence (vLLM sleep is dev-mode-only; Ollama exposes no external
priority/preempt API; MPS priority is a hint; MIG is static partitions, not dynamic
eviction). Do not let this creep back in.

## Consider not building this at all: use llama-swap

llama-swap already implements reliable process hot-swapping for any OpenAI/Anthropic-
compatible local server (llama.cpp, vLLM, …), with unload endpoints, running-model
status, TTL, groups, logs, and a web UI. Two viable designs:

- **(A) Drive llama-swap** as the swap executor for owned engines; LocalHub owns
  inventory, compatibility, supply-chain, metrics, and the phone UI on top. Cuts a whole
  phase of process-supervision code and reuses a battle-tested swapper.
- **(B) Reimplement** a minimal residency manager (spec below) if you want a single
  binary with no external swapper dependency, or need eviction policy llama-swap doesn't
  expose.

Decide explicitly in [ADR-003](decisions.md#adr-003). Default recommendation: prototype
with (A) in Phase 0/5, only build (B) if a concrete limitation forces it.

## The algorithm (if you build B)

Conservative, coarse, and legible. Runs on model-load requests.

```
admit(model_id, engine, priority):
    inst = running_instance(model_id, engine)
    if inst and inst.state == HEALTHY:
        touch(inst); return inst                       # already warm

    need = estimate_vram(model_id, engine)             # from compatibility engine
    free = gpu_free_vram()                              # NVML, live

    while free < need + SAFETY_MARGIN:
        victim = pick_victim(exclude_active=True)
        if victim is None:
            if priority >= HIGH:
                return reject("insufficient VRAM, no evictable model")
            else:
                enqueue(model_id, engine, priority, blocked_reason="waiting for VRAM")
                return WAITING
        evict(victim)                                   # engine.stop() (or sleep, if enabled)
        free = gpu_free_vram()                           # re-measure; don't trust bookkeeping

    inst = engine.start(model_id, launch_params)
    wait_healthy(inst)
    return inst
```

### `pick_victim` ordering

Evict in this order, skipping anything ineligible:

1. **Expired TTL** — idle longer than its `ttl_seconds`.
2. **Idle + lowest priority** — no active requests, lowest `priority`, then LRU
   (`last_used_at`).
3. **Largest reclaimable** — only if still short after 1–2, prefer freeing the most VRAM.

**Never evict:**
- `pinned = 1`.
- any instance with **active in-flight requests** (mark `DRAINING`, wait, don't kill).
- the instance we're trying to make room *for* (obviously).

### Critical rule: re-measure, never trust bookkeeping

After each eviction, read **actual free VRAM from NVML**, not a running counter. Engines
free asynchronously (especially Ollama's `keep_alive: 0`), fragmentation exists, and
other processes may be using the GPU. Loading based on a bookkeeping estimate that says
"there's room" when there isn't = the exact OOM this product promises to prevent.

### Never-kill-active-streams

Default behavior: an instance with active generations is `DRAINING`, not killed. Only
after in-flight requests complete (or a drain timeout the user explicitly opted into)
does it stop. This is a core promise from the product brief. A config flag can allow
force-eviction for a specific high-priority request, off by default, with a loud warning.

## Concurrency across engines on one GPU

When Ollama, llama.cpp, and vLLM each hold VRAM on the same GPU, there is **no shared
allocator** — each has its own CUDA context. The residency manager's only lever is *how
much each is allowed to be resident*, enforced by loading/unloading whole models and by
each engine's own memory knobs (`--gpu-memory-utilization` for vLLM, `-ngl` for
llama.cpp, `OLLAMA_MAX_LOADED_MODELS` for Ollama). Budget conservatively; leave headroom;
re-measure. You cannot make three engines cooperatively share a fluid VRAM pool — you can
only keep the sum of their whole-model footprints under the card.

## Queue semantics (coarse)

The `queue` table backs a simple WAITING→ADMITTED flow, not a fair scheduler:

- A blocked low-priority request waits until eviction frees room or a running model exits.
- `blocked_reason` is always populated and shown in the UI ("waiting: 3.2 GB short,
  no evictable model").
- No starvation guarantees in v1; document this. If it bites, add simple aging later.

## What success looks like

- Load-storm test: rapidly request 6 models that can't all fit on a 16 GB card. Result:
  no OOM, no killed active streams, sensible evictions, clear blocked reasons — never a
  crash and never a silent wrong answer.
