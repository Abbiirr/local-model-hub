# Engine Adapters

Each engine implements one contract. The hard part is that the three engines have
**fundamentally different ownership models**, and the adapter layer must not paper over
that — it must expose it, because residency behavior depends on it.

## The ownership asymmetry (read first)

| Engine | Who owns the process | How we start a model | How we stop/free VRAM | Ports |
|---|---|---|---|---|
| **Ollama** | Ollama daemon (shared, not ours) | warm via API call | `keep_alive: 0` (request unload) | one daemon, one port |
| **llama.cpp** | **LocalHub** (we spawn `llama-server`) | spawn process | SIGTERM the process | one port **per model** |
| **vLLM** | **LocalHub** (we spawn `vllm serve`) | spawn process/container | SIGTERM (or sleep, advanced) | one port **per model** |

Consequence: for llama.cpp/vLLM, "unload" is deterministic and immediate (kill the
process, VRAM is reclaimed by the OS). For Ollama, "unload" is a *request* to a daemon we
don't control; it happens on the daemon's schedule.

## Common contract

```
interface EngineAdapter:
  detect()                  -> { installed: bool, version: str, running: bool, base_url: str? }
  list_models()             -> [ ModelRef ]           # what this engine can serve
  list_running()            -> [ RunningInstance ]     # what's resident now (+vram, +expiry)
  start(model, params)      -> Instance                # idempotent if already running
  stop(instance)            -> ok                       # best-effort free of VRAM
  health(instance)          -> HEALTHY | STARTING | UNHEALTHY
  supports_sleep()          -> bool                     # true only for specially-launched vLLM
  openai_base_url(instance) -> str                      # where the thin proxy routes
```

`params` is engine-specific but always includes: `ctx`, and for owned engines `port`,
plus the engine's memory knobs.

---

## Ollama adapter

Ollama is a shared daemon. We observe and nudge it; we never spawn per-model processes.

- **detect:** `GET {base}/api/version` (default base `http://127.0.0.1:11434`).
- **list_models:** `GET /api/tags` → installed models with sizes/digests.
- **list_running:** `GET /api/ps` → currently loaded models **with VRAM size and
  `expires_at`**. This is your source of truth for Ollama residency.
- **start (warm):** issue a minimal `POST /api/generate` (or `/api/chat`) with the model
  and `keep_alive` set. There is no explicit "load" endpoint; a request warms it.
- **stop (unload):** `POST /api/generate` with `{"model": M, "keep_alive": 0}` requests
  immediate unload. `keep_alive: -1` keeps it resident indefinitely (use for pinned).
- **health:** model appears in `/api/ps` and responds.

### Coarse controls (env, set on the Ollama daemon)

| Var | Effect |
|---|---|
| `OLLAMA_MAX_LOADED_MODELS` | cap concurrent resident models |
| `OLLAMA_NUM_PARALLEL` | concurrent requests per model |
| `OLLAMA_KEEP_ALIVE` | default idle unload timer |
| `OLLAMA_MODELS` | model storage path (for inventory) |

### Limits to encode honestly

- Ollama's own FAQ: for GPU inference, a new model must **fully fit in VRAM** to load
  alongside existing ones; otherwise it may swap. This is not an external
  priority/preempt API — it's coarse.
- `keep_alive: 0` is a request; unload timing is the daemon's. Poll `/api/ps` to confirm
  the VRAM is actually freed before assuming headroom.

---

## llama.cpp adapter (`llama-server`)

LocalHub **owns** each process. This is where most residency logic lives, and it's
essentially the llama-swap pattern (see [ADR-003](decisions.md#adr-003) on possibly using
llama-swap directly).

- **detect:** `llama-server --version` (locate binary; store in `engines.binary_path`).

  > **Backends are inferred from disk when `--version` says nothing (2026-07-25).** Modern
  > llama.cpp loads `ggml-cuda.dll` / `.so` / `.dylib` as a *dynamic* backend, so its
  > `--version` banner prints only the build number — no backend line at all. b10106 printed
  > `version: 10106 (1425386fd)` + `built with Clang`, and nothing else. The old CUDA fork does
  > print `ggml_cuda_init: found 1 CUDA devices`. Parsing the banner alone therefore reported a
  > real CUDA build as backend-less. `llamacpp.detectOne` now falls back to `backendsFromDir`,
  > which infers backends from the `ggml-*` shared libraries beside the executable.
  > Live result: b10106 reports `backends: [CPU, CUDA, RPC]` (previously `null`).
  > **Do not treat a missing backend banner as "no GPU support"** — re-verify against the
  > binary's actual shipped libraries.

- **binary selection is capability-first, then CONFIGURED ORDER — never `has_cuda`.**
  `compat.bestLlamaBinary` takes a `BinaryRequirements` filter (e.g. `TQ3`, `SpecMTP`) and
  returns the first *detected* candidate in the order the operator configured. It previously
  preferred any binary whose `has_cuda` was true, which silently overrode config order: a
  modern build detected as `has_cuda=false` (see above) **lost to an older CUDA fork**, and that
  fork then failed to load a newer architecture outright —
  `llama_model_load: error loading model: missing tensor 'blk.64.ssm_conv1d.weight'` for
  Qwen3.6's hybrid SSM/attention arch. `HasCUDA` is now an accurate **reporting** field only.
  Consequence for operators: **the order of `engines.llamacpp.binary_paths` is meaningful** —
  list the preferred build first; capability-gated fallbacks (the `has_tq3` fork) still win
  automatically for models that require them.
- **start:** spawn, bound to localhost, on an allocated port:

  ```bash
  llama-server -m /models/qwen2.5-coder-7b-q4_k_m.gguf \
    --host 127.0.0.1 --port 8081 \
    -c 8192 -ngl 99 --flash-attn \
    --no-webui
  ```

  Record PID, port, and the exact flags in `instances.launch_flags_json`.
- **health:** `GET http://127.0.0.1:PORT/health`.
- **list_running:** LocalHub's own `instances` table (we own them), reconciled by
  checking each PID + `/health`.
- **stop (free VRAM):** SIGTERM the PID; on timeout, SIGKILL. VRAM reclaimed by OS on exit.
- **openai_base_url:** `http://127.0.0.1:PORT/v1`.

Notes: `-ngl 99`/`auto` offloads all layers to GPU; the compatibility engine decides the
right value (and whether CPU offload is needed). One model = one process = one port;
LocalHub allocates ports from a pool.

---

## vLLM adapter

LocalHub **owns** the process (or container). Highest capability, highest cost.

- **detect:** `vllm --version` (or presence of the container image).
- **start:** spawn `vllm serve`, bound to localhost:

  ```bash
  vllm serve Qwen/Qwen2.5-Coder-7B-Instruct \
    --host 127.0.0.1 --port 8091 \
    --gpu-memory-utilization 0.90 \
    --max-model-len 8192 \
    --dtype auto
    # --tensor-parallel-size N  (multi-GPU only)
  ```

- **health:** `GET http://127.0.0.1:PORT/health`.
- **stop:** SIGTERM; VRAM reclaimed on exit.
- **openai_base_url:** `http://127.0.0.1:PORT/v1`.

### Cold start is slow — surface it

vLLM startup includes weight load + CUDA graph capture; first-ready can be tens of
seconds. The UI must show a `STARTING` state with a spinner and rough ETA, or users will
think it hung. This is a UX requirement, not a nicety.

### Sleep mode: advanced, opt-in, NOT default

vLLM has `/sleep` and `/wake_up` endpoints, but:

- Only exist when launched with **`VLLM_SERVER_DEV_MODE=1 --enable-sleep-mode`**.
- vLLM docs explicitly say these dev endpoints **should not be exposed to users** and
  only in trusted networks.
- **Level 1** offloads weights to CPU RAM (needs CPU RAM ≥ weights) and discards KV.
  **Level 2** discards weights too.

So: `supports_sleep()` returns true only for instances LocalHub deliberately launched in
this mode, LocalHub keeps those endpoints bound to localhost and never proxies them to
clients, and default vLLM residency is **process stop**, not sleep. Sleep is a
power-user toggle for fast A↔B switching on one GPU, gated behind a clear warning. See
[residency-scheduler.md](residency-scheduler.md) and [ADR-001](decisions.md#adr-001).

### GGUF on vLLM: don't route here

vLLM's own docs call GGUF support experimental/under-optimized and it has moved out of
core. **Route GGUF to llama.cpp/Ollama, never to vLLM.** The compatibility engine already
encodes this; the adapter should refuse a GGUF start with a clear message.

---

## Adapter test checklist (per engine)

1. `detect` correctly reports installed/version/running on a clean and a configured box.
2. `start` → `health` transitions to HEALTHY and the model actually answers.
3. `stop` actually frees VRAM (verify via NVML delta, not just process exit).
4. `list_running` matches reality after start/stop churn.
5. Reconciliation after control-plane restart: owned processes re-attached or reaped;
   Ollama re-mirrored from `/api/ps`.
