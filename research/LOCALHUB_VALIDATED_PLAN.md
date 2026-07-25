# LocalHub Validated Plan

## Decision

Do **not** build the original broad LocalHub as a direct competitor to GPUStack, Xinference, LocalAI, Open WebUI, LiteLLM, and llama-swap.

Build a narrower and more defensible product:

> **LocalHub Compass** — a single-box model inventory, hardware compatibility, reproducible model supply-chain, engine-version pinning, monitoring, and coarse model residency/orchestration layer for Ollama, llama.cpp, and vLLM.

The product can still include a dashboard, model download, model launch, model health, queue status, GPU status, and Tailscale mobile access. The main change is that it should not claim to provide a live cross-engine VRAM fair-share scheduler or a universal model-conversion pipeline.

---

## Validated Corrections to the Original Plan

### Keep

- Unified dashboard for local GPU status, model inventory, engine status, queue status, and health.
- Support for Ollama, llama.cpp / llama-server, and vLLM.
- Model discovery from local engine registries and configured model folders.
- Hardware compatibility checks using GPU, driver, compute capability, VRAM, RAM, OS, and backend requirements.
- Model download from Hugging Face, Ollama registry, and direct GGUF repositories.
- OpenAI-compatible northbound endpoint, but only as a thin router/proxy, not a new full gateway competing with LiteLLM/GPUStack/LocalAI.
- Priority queue and admission control, but at process/model-load level.
- Tailscale-secured mobile dashboard and monitoring.

### Cut or downgrade

- **Cut live cross-engine VRAM preemption.** Current engines do not expose the cooperative memory hooks needed for that.
- **Cut automatic Safetensors-to-GGUF conversion as a core feature.** Treat formats as engine-native. Link to conversion tooling only as an advanced manual workflow.
- **Cut new full chat UI as a core feature.** Use Open WebUI or another frontend for chat. LocalHub’s UI should be operations-focused.
- **Cut cluster/Kubernetes scope from MVP.** Start single machine. Multi-node comes only if single-box adoption proves demand.
- **Downgrade Tailscale from mandatory spine to recommended remote-access mode.** The app should run without Tailscale, but Tailscale should be the safest default remote story.

---

## Product Shape

### Product Name

**LocalHub Compass**

### Target User

Developers and local-AI power users who run several local models across Ollama, llama.cpp, and vLLM on one workstation and want to know:

- What models are installed?
- Which models can run on this hardware?
- Which engine/version should run each model?
- What is using VRAM right now?
- What should be unloaded before starting another model?
- Can I monitor/control this from my phone over Tailscale?

### Primary Wedge

The defensible wedge is **compatibility and reproducibility**, not generic inference hosting.

LocalHub Compass should become the thing that says:

> “For this exact GPU, driver, CUDA/ROCm version, model format, quantization, context length, and engine version, this model is likely to run / not run / run with warnings.”

---

## System Architecture

```text
                           Phone / Browser
                                |
                         Tailscale / LAN
                                |
                   +------------v-------------+
                   | LocalHub Web UI / API    |
                   | REST + WebSocket/SSE     |
                   +------------+-------------+
                                |
            +-------------------+-------------------+
            |                                       |
+-----------v------------+              +-----------v------------+
| Control Plane          |              | Metrics Collector       |
| - inventory            |              | - NVML / ROCm SMI       |
| - compat rules         |              | - CPU/RAM/disk          |
| - model manifests      |              | - engine health         |
| - queue/admission      |              | - token/s parsing       |
| - engine adapters      |              +------------------------+
+-----------+------------+
            |
   +--------+---------+-------------------+
   |                  |                   |
+--v---------+   +----v----------+   +----v------+ 
| Ollama     |   | llama-server  |   | vLLM      |
| adapter    |   | adapter       |   | adapter   |
+------------+   +---------------+   +-----------+
```

---

## Recommended Stack

### MVP stack

- **Backend:** Go or Python/FastAPI.
  - Go is better for a single binary, process supervision, Tailscale `tsnet`, and long-running daemon behavior.
  - Python/FastAPI is faster for first implementation, Hugging Face metadata handling, and quick UI/API iteration.
- **Recommended practical path:** Python/FastAPI for prototype; migrate host agent to Go if packaging becomes important.
- **Frontend:** React + Vite + TanStack Query + WebSocket/SSE stream.
- **Database:** SQLite first. Use WAL mode. No Postgres for MVP.
- **Queue state:** SQLite-backed priority queue; no Redis for MVP.
- **Metrics:** Prometheus-compatible `/metrics` endpoint plus live WebSocket for UI.
- **GPU metrics:** NVML for NVIDIA; ROCm SMI for AMD; CPU/RAM via psutil/gopsutil.
- **Packaging:** Docker Compose plus bare-metal mode. Bare-metal matters because users already have local Ollama/llama.cpp/vLLM installs.
- **Remote access:** Tailscale Serve or embedded tsnet if using Go.

---

## Core Modules

### 1. Hardware Inventory Module

Collect:

- OS, kernel, architecture.
- CPU model, cores, RAM, swap.
- GPU name, VRAM, driver version, compute capability.
- CUDA runtime/driver version if present.
- ROCm version if present.
- Metal/Vulkan support if later expanded.
- Current GPU utilization, memory used/free, temperature, power.

Output example:

```json
{
  "host_id": "desktop-4060ti",
  "gpus": [
    {
      "vendor": "nvidia",
      "name": "RTX 4060 Ti",
      "vram_total_mb": 16380,
      "compute_capability": "8.9",
      "driver_version": "...",
      "cuda_supported": true
    }
  ]
}
```

### 2. Engine Discovery Module

Supported engines in MVP:

- Ollama
- llama.cpp / llama-server
- vLLM

For each engine, discover:

- Installed binary/container.
- Version.
- Listening port.
- Running processes.
- Model registry or configured model paths.
- Health endpoint.
- Supported API shape.

Discovery methods:

- Ollama: `/api/tags`, `/api/ps`, `ollama list`, `ollama ps`.
- llama.cpp: configured process definitions, `/health`, `/props`, `/v1/models` if available.
- vLLM: process registry started by LocalHub, `/v1/models`, health endpoint, config file.

### 3. Model Inventory Module

Represent every model as an artifact with normalized metadata:

```json
{
  "model_id": "qwen2.5-coder-7b-instruct-q4km",
  "source": "huggingface",
  "repo": "Qwen/Qwen2.5-Coder-7B-Instruct-GGUF",
  "revision": "commit_sha",
  "format": "gguf",
  "quantization": "Q4_K_M",
  "size_bytes": 4680000000,
  "license": "apache-2.0",
  "sha256": "...",
  "local_path": "/models/qwen/...gguf"
}
```

### 4. Compatibility Module

Compatibility result states:

- `RUNS`
- `RUNS_WITH_WARNINGS`
- `CPU_ONLY`
- `PARTIAL_OFFLOAD_REQUIRED`
- `UNSUPPORTED_ENGINE`
- `UNSUPPORTED_HARDWARE`
- `INSUFFICIENT_VRAM`
- `UNKNOWN_NEEDS_TEST`

Rules to include:

- Ollama NVIDIA compute capability and driver requirements.
- vLLM CUDA/Linux constraints and model-family support.
- vLLM model format support by version.
- llama.cpp broad GGUF support and CPU/GPU offload options.
- Estimated model VRAM footprint.
- Estimated KV cache footprint based on context length, batch/concurrency, hidden size, layer count, dtype when metadata is available.
- Runtime policy warnings: large context, multiple parallel requests, GPU memory fragmentation, split CPU/GPU offload.

### 5. Supply-Chain / Reproducibility Module

Core features:

- Pin Hugging Face models by exact commit hash.
- Store local immutable manifest.
- Record source URL, revision, file list, file hashes, license, model card snapshot date.
- Prefer Safetensors for vLLM/PyTorch artifacts.
- Warn on pickle files and `trust_remote_code`.
- Warn when model license is unknown, gated, or incompatible with redistribution.
- Support an internal mirror path, but do not become a full model registry in MVP.

### 6. Download Module

Supported downloads:

- Hugging Face snapshot or selected files.
- Direct GGUF file URLs.
- Ollama `pull`.

Required behavior:

- Resume downloads.
- Hash verification where possible.
- Disk-space preflight.
- License/gated-model warning before download.
- Store manifest before and after download.

### 7. Engine Adapter Module

Adapter interface:

```ts
interface EngineAdapter {
  detect(): EngineInstall[]
  listModels(): ModelRef[]
  health(): EngineHealth
  estimateRunConfig(model, hardware, policy): RunPlan
  start(model, runConfig): ModelInstance
  stop(instanceId): void
  unload(modelId): void
  proxy(request): ResponseStream
}
```

Adapter details:

#### Ollama

- Use native model registry.
- Launch/unload through Ollama API or CLI.
- Use `keep_alive`, `ollama stop`, `OLLAMA_MAX_LOADED_MODELS`, `OLLAMA_NUM_PARALLEL`, and queue settings where available.
- Do not claim precise external priority control.

#### llama.cpp / llama-server

- Manage process lifecycle directly.
- Generate command line from config.
- Control GPU layers, context length, batch size, port, model path.
- Stop process to unload model.
- This is the easiest engine for LocalHub to supervise precisely.

#### vLLM

- Manage vLLM server process/container.
- Generate command line with model path/repo, dtype, quantization, tensor parallel size, max model length, GPU memory utilization.
- Treat sleep/wake only as optional advanced optimization, not a production preemption API.

### 8. Queue and Residency Module

This is **not** a cross-engine VRAM fair-share scheduler.

It is a conservative admission-control and model-residency layer.

Request fields:

```json
{
  "request_id": "...",
  "model_id": "...",
  "priority": 100,
  "tenant": "local-user",
  "stream": true,
  "max_tokens": 2048,
  "deadline_ms": null,
  "preemptible": false
}
```

Model states:

- `DOWNLOADED`
- `VERIFIED`
- `COLD`
- `STARTING`
- `WARM`
- `RUNNING`
- `DRAINING`
- `STOPPING`
- `STOPPED`
- `FAILED`

Scheduling behavior:

1. Check whether target model is already warm.
2. If warm and healthy, route immediately.
3. If cold, estimate whether it can fit in current free VRAM/RAM.
4. If not enough memory, identify unload candidates.
5. Prefer unloading idle, low-priority, expired-TTL models.
6. Do not kill in-flight generations unless request is explicitly preemptible.
7. Start target model.
8. Route request to backend.
9. Record token/s, latency, memory delta, and errors.

Eviction score:

```text
eviction_score =
  idle_bonus
  + ttl_expired_bonus
  + low_priority_bonus
  + large_vram_reclaim_bonus
  - active_request_penalty
  - pinned_model_penalty
  - warm_pool_penalty
```

### 9. API Layer

LocalHub should expose two API categories.

#### Management API

- `GET /api/hosts/current`
- `GET /api/gpus`
- `GET /api/engines`
- `GET /api/models`
- `POST /api/models/download`
- `POST /api/models/verify`
- `POST /api/models/{id}/compatibility-check`
- `POST /api/instances/start`
- `POST /api/instances/{id}/stop`
- `GET /api/queue`
- `POST /api/queue/pause`
- `GET /api/metrics/live`
- `GET /metrics`

#### Thin OpenAI-Compatible Proxy

- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/completions`
- Optional later: embeddings, rerank, responses.

The OpenAI-compatible layer should be thin. For serious gateway needs, integrate LiteLLM or llama-swap instead of rebuilding everything.

### 10. UI Plan

Pages:

1. **Home / GPU status**
   - VRAM used/free.
   - GPU utilization.
   - Temperature and power.
   - Running models.
   - Queue length.
   - Active generations.

2. **Models**
   - Installed models.
   - Source, format, quantization, size, license.
   - Engine compatibility badges.
   - “Run with Ollama / llama.cpp / vLLM” recommendations.

3. **Download / Catalog**
   - Hugging Face URL/repo input.
   - Ollama model input.
   - Direct GGUF URL input.
   - Manifest preview.
   - Compatibility preview before download.

4. **Engines**
   - Installed engine versions.
   - Running ports.
   - Health.
   - Suggested engine pin.
   - Known compatibility warnings.

5. **Queue**
   - Pending requests.
   - Priority.
   - Target model.
   - Estimated start reason.
   - Blocked reason: insufficient VRAM, model loading, engine unavailable, etc.

6. **Mobile view**
   - GPU status.
   - Running models.
   - Stop/unload buttons.
   - Queue status.
   - Alerts.

---

## Database Schema

```sql
CREATE TABLE hosts (
  id TEXT PRIMARY KEY,
  hostname TEXT,
  os TEXT,
  arch TEXT,
  created_at TEXT,
  updated_at TEXT
);

CREATE TABLE devices (
  id TEXT PRIMARY KEY,
  host_id TEXT,
  vendor TEXT,
  name TEXT,
  vram_total_mb INTEGER,
  compute_capability TEXT,
  driver_version TEXT,
  runtime_version TEXT,
  metadata_json TEXT
);

CREATE TABLE engine_installs (
  id TEXT PRIMARY KEY,
  engine TEXT,
  version TEXT,
  install_type TEXT,
  binary_path TEXT,
  base_url TEXT,
  status TEXT,
  metadata_json TEXT
);

CREATE TABLE model_artifacts (
  id TEXT PRIMARY KEY,
  name TEXT,
  source TEXT,
  repo TEXT,
  revision TEXT,
  format TEXT,
  quantization TEXT,
  local_path TEXT,
  size_bytes INTEGER,
  sha256 TEXT,
  license TEXT,
  manifest_json TEXT,
  created_at TEXT
);

CREATE TABLE compatibility_results (
  id TEXT PRIMARY KEY,
  model_id TEXT,
  engine TEXT,
  engine_version TEXT,
  device_id TEXT,
  status TEXT,
  estimated_vram_mb INTEGER,
  reasons_json TEXT,
  checked_at TEXT
);

CREATE TABLE model_instances (
  id TEXT PRIMARY KEY,
  model_id TEXT,
  engine TEXT,
  port INTEGER,
  pid INTEGER,
  state TEXT,
  priority INTEGER,
  started_at TEXT,
  last_used_at TEXT,
  run_config_json TEXT
);

CREATE TABLE queue_jobs (
  id TEXT PRIMARY KEY,
  model_id TEXT,
  priority INTEGER,
  state TEXT,
  request_json TEXT,
  blocked_reason TEXT,
  created_at TEXT,
  started_at TEXT,
  finished_at TEXT
);

CREATE TABLE request_logs (
  id TEXT PRIMARY KEY,
  queue_job_id TEXT,
  model_id TEXT,
  engine TEXT,
  prompt_tokens INTEGER,
  completion_tokens INTEGER,
  latency_ms INTEGER,
  tokens_per_second REAL,
  error TEXT,
  created_at TEXT
);

CREATE TABLE audit_log (
  id TEXT PRIMARY KEY,
  actor TEXT,
  action TEXT,
  target TEXT,
  metadata_json TEXT,
  created_at TEXT
);
```

---

## Phased Roadmap

### Phase 0 — Use Existing Tools Immediately

For personal use today:

- Use GPUStack if you want model deployment, compatibility checks, monitoring, and OpenAI-compatible APIs.
- Use llama-swap if you want simple local hot-swapping over llama.cpp/vLLM/OpenAI-compatible backends.
- Use Open WebUI for chat.
- Use Tailscale Serve to access the dashboard from phone.

This proves the workflow before building a new product.

### Phase 1 — LocalHub Compass MVP

Build only:

- Hardware detection.
- Engine detection.
- Model inventory.
- Compatibility checks.
- Tailscale/LAN dashboard.
- Manual start/stop/unload.
- Read-only monitoring.

No queue yet. No model download yet.

### Phase 2 — Reproducible Model Supply Chain

Add:

- Hugging Face revision pinning.
- Manifest generation.
- File hash verification.
- License warning.
- Safetensors preference.
- Pickle / trust_remote_code warnings.
- Direct GGUF and Ollama pull support.

### Phase 3 — Coarse Residency + Queue

Add:

- Priority queue.
- Admission control.
- Whole-model unload decisions.
- Warm pool.
- TTL expiry.
- No in-flight preemption by default.

Benchmark against llama-swap. If LocalHub cannot beat it for local workflows, upstream improvements to llama-swap or become a companion dashboard.

### Phase 4 — Thin Proxy / Gateway

Add minimal OpenAI-compatible routing only if needed:

- `/v1/models`
- `/v1/chat/completions`
- Streaming support.
- Queue integration.

For advanced gateway features, integrate LiteLLM.

### Phase 5 — Optional Multi-Node

Only after single-box demand is proven:

- Multiple LocalHub agents.
- Central controller.
- Worker labels.
- Placement policy.
- Prometheus/Grafana integration.

At this point, compare again against GPUStack before building. If GPUStack covers the need, contribute instead.

---

## Implementation Milestones

### M1: Read-only dashboard

- Detect NVIDIA GPU through NVML.
- Show VRAM, utilization, temperature, power.
- Detect Ollama models.
- Detect llama.cpp configured models.
- Detect vLLM configured models.
- Show installed/running model table.

### M2: Compatibility engine

- Add static rules for Ollama, llama.cpp, vLLM.
- Estimate model memory.
- Generate run recommendations.
- Store compatibility results.

### M3: Controlled launch

- Start/stop llama-server.
- Start/stop vLLM server.
- Trigger Ollama pull/run/stop through API/CLI.
- Record model instance state.

### M4: Supply-chain manifests

- Download from Hugging Face.
- Pin revision.
- Hash files.
- Store manifest.
- Show security warnings.

### M5: Queue/admission

- Add priority queue.
- Add cold/warm/running state machine.
- Add whole-model eviction.
- Add request logs and token/s.

### M6: Tailscale production mode

- Bind backend to localhost by default.
- Add Tailscale Serve instructions.
- If Go agent exists, add optional tsnet mode.
- Add identity-header based auth option.

---

## Final Recommendation

Build LocalHub Compass as a **single-machine compatibility, inventory, supply-chain, monitoring, and coarse residency manager**.

Do not build a full GPUStack clone. Do not market cross-engine VRAM scheduling. Do not own auto conversion. The strongest product is the one that tells the user exactly what can run, why, with which engine, and what must be unloaded first.
