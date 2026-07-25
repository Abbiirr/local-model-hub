# 21 — Observability Spec

## Metrics to collect

Hardware:

- GPU VRAM used/free/total.
- GPU utilization.
- GPU temperature.
- GPU power draw.
- Driver and CUDA/ROCm version.
- CPU utilization.
- RAM used/free.
- Disk used/free for model directory.

Engine:

- installed/running/version.
- health endpoint status.
- base URL.
- models known.
- running models.

Instance:

- state.
- PID/port/base URL for owned engines.
- active requests.
- tokens/s if available.
- TTFT if available.
- errors.
- last-used timestamp.
- estimated and observed VRAM.

Queue:

- waiting count.
- blocked reason.
- priority.
- vram estimate.
- age.

## Endpoints

- `GET /api/metrics`
- `GET /api/metrics/live` SSE
- `GET /metrics` Prometheus text format
- `GET /api/instances/{id}/logs` SSE

## Logging rules

- Log every launch command with secrets redacted.
- Log every compatibility verdict and estimator version.
- Log every eviction decision with candidate list and reason.
- Log every rejected request with blocked reason.
- Do not log prompts or generated text by default.
