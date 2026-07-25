# X4.1 local-model acceptance campaign — 2026-07-11

## Scope and safety record

Ran the currently discovered local inventory through a temporary LocalHub daemon on
`127.0.0.1:39950`. Loads were sequential. Every started instance was stopped through
LocalHub and recovery was checked with NVML. At close-out no `llama-server`, `ollama`, or
campaign LocalHub process remained; NVML was **615 MiB used / 15495 MiB free** and free host
RAM was **9722 MiB**.

The prior direct vLLM smoke is not counted as a LocalHub vLLM result here. X4.1 remains open.

## Results

| Local model / route | Result | Evidence and classification |
|---|---|---|
| Llama 3.2 1B GGUF / llama.cpp | LIVE PASS | HEALTHY; chat relay returned `LOCALHUB_1B_OK`; stop verified +1286 MiB free VRAM. |
| Gemma 4 12B Q4 GGUF / llama.cpp | LIVE PASS | HEALTHY; chat relay HTTP 200; stop verified +9039 MiB. |
| GLM-OCR Q8 GGUF / llama.cpp | LIVE PASS (text) | HEALTHY; relay HTTP 200; stop verified +2045 MiB. Image payload still required. |
| PaddleOCR-VL GGUF / llama.cpp | LIVE PASS (text) | HEALTHY; relay HTTP 200; stop verified +2061 MiB. Image payload still required. |
| Qwen3-VL-Embedding-8B Q6 GGUF / llama.cpp | BLOCKED | HEALTHY and unload verified (+6959 MiB), but no suitable LocalHub embedding inference was captured. |
| nomic embedding GGUF / llama.cpp | BLOCKED | HEALTHY and unload verified (+310 MiB); chat relay HTTP 500 is not an embedding path. |
| Qwen3-VL-32B Q2 GGUF / llama.cpp | BLOCKED (safety stop) | At ctx 2048 it reached 14052 MiB observed footprint but was not healthy and host free RAM fell below 1 GiB. Stop recovered +14050 MiB. Not a `runs` pass. |
| Qwen3.5 27B TQ3 GGUF | VERIFIED REFUSAL | LocalHub HTTP 409 `wont_run` at ctx 4096; no engine spawned. |
| Qwen3.6 27B TQ3 GGUF | VERIFIED REFUSAL | HTTP 409: estimated 15326 MiB versus usable roughly 14209 MiB; no engine spawned. |
| Qwopus TQ3 vision GGUF | VERIFIED REFUSAL | Safe ctx-4096 capacity refusal; no engine spawned. |
| Qwen3.6 35B-A3B TQ3 GGUF | BLOCKED (defect) | `runs_degraded` selected unrelated root `mmproj-BF16.gguf`; one launch crashed and a later launch became healthy with that wrong projector. Both stopped; latter recovery +14416 MiB. |
| Ollama nomic embedding | PARTIAL PASS | LocalHub warm became HEALTHY; native `/api/embed` returned 768 dimensions. LocalHub has no embedding relay. |
| Ollama DeepSeek-R1/Qwen3 8B | LIVE PASS | LocalHub warm plus chat relay HTTP 200; generation began and was token-limit truncated. |
| Ollama GLM-OCR | VERIFIED FAILURE | Warm failed with Ollama HTTP 500: model failed to load. |
| Ollama Qianfan-OCR 4B | LIVE PASS | LocalHub warm and chat relay HTTP 200. |
| Ollama MiniCPM-O4.5 | VERIFIED FAILURE | Warm failed: `llama runner process has terminated`. |
| Safetensors / vLLM | BLOCKED | No currently local Safetensors model was configured for a LocalHub route. A direct temporary vLLM Qwen 0.6B smoke does not satisfy X4.1. |

## Findings that keep X4.1 open

1. LocalHub lacks a suitable embedding relay.
2. Text-only relays do not prove OCR or vision behavior; every vision row needs an image request.
3. The 35B-A3B model was paired with an unrelated projector, making its verdict untrustworthy.
4. The 32B VL load needs a conservative refusal or a proven lower-footprint configuration; it must not be forced.
5. A real Safetensors model must be configured and proven through LocalHub's vLLM adapter.

## Full-VRAM TQ3 follow-up — 2026-07-11

At the owner's request, Docker Desktop was stopped temporarily (there were no active
containers) to make host RAM available, then each TQ3 model was tried directly with the
CUDA TQ3 llama.cpp fork, `-ngl all`, `--fit off`, Flash Attention, and ctx 512. This is a
capacity calibration experiment, not a reason to bypass LocalHub admission in normal use.

| Model | Result | Measured result |
|---|---|---|
| Qwen3.5 27B TQ3 | DOES NOT LOAD | Reached about 13.8–14.1 GiB VRAM but remained HTTP 503/not-ready while host free RAM fell to 327–389 MiB, then exited. |
| Qwen3.6 27B TQ3 + its own projector | HEALTHY, INFERENCE BLOCKED | Reached `/health` 200 using 15985 MiB VRAM (125 MiB free). A 4-token completion did not return within 90 seconds. Process was stopped; recovery to 1266 MiB used / 12009 MiB free RAM. |
| Qwopus 27B TQ3 + its own projector | HEALTHY, INFERENCE BLOCKED | Reached `/health` 200 using 15964 MiB VRAM (146 MiB free). A 4-token completion did not return within 60 seconds. Process was stopped; recovery to 1226 MiB used / 11571 MiB free RAM. |

This narrows the estimator work: its ctx-4096 refusals are appropriate for normal use, but
it should expose a clearly marked low-context full-VRAM `runs_degraded` path for the latter
two models only after the completion stall is diagnosed and a usable latency bound is proven.

## Owner constraint — 2026-07-11

Do not load or benchmark models of **30B parameters or larger** on this box. The Qwen3-VL-32B
and Qwen3.6-35B-A3B entries are estimate/refusal-only from this point forward. A future trial
requires explicit owner reversal of this constraint.

## Ornith AWQ vLLM probe — 2026-07-11

Tried `cyankiwi/Ornith-1.0-9B-AWQ-INT4` in `vllm/vllm-openai:latest` (vLLM 0.19.1) with
`--max-model-len 4096`, `--gpu-memory-utilization 0.75`, and `--trust-remote-code`. vLLM
recognized the repository as `Qwen3_5ForConditionalGeneration` with
`quantization=compressed-tensors`, so this was not an immediate format rejection.

The probe was stopped before weight load/completion: Docker's WSL VM reduced free host RAM to
487 MiB while vLLM was initializing, with GPU still near baseline (~1287 MiB used). Container
and Docker Desktop were stopped; no Ornith inference verdict is claimed. The next safe attempt
requires freeing more host RAM before starting Docker, then proving `/health` and one completion.
