# 16 — Implementation Playbook

## Recommended build sequence

Do not start with the proxy or scheduler. Start with the safest useful thing.

1. Phase 0 field validation.
2. Phase 1 read-only daemon + dashboard.
3. Phase 2 compatibility preview.
4. Phase 3 manifest/download.
5. Phase 4 controlled run/stop.
6. Phase 5 coarse residency.
7. Phase 6 thin proxy only if needed.

## Stack decision

Default product stack:

- Control plane: Go.
- DB: SQLite WAL.
- Frontend: React + Vite, bundled as static assets.
- Metrics: NVML first, then ROCm/Apple later.
- Remote access: localhost bind + Tailscale Serve; optional tsnet if Go integration is worth the auth/setup tradeoff.

Prototype exception:

Python/FastAPI is allowed only if the goal is a fast Phase 1–2 demo. If chosen, create an ADR with the date and a concrete Go migration trigger.

## Repository shape

```text
cmd/localhub/              daemon entrypoint
internal/api/              REST + SSE handlers
internal/db/               migrations + queries
internal/hardware/         NVML/ROCm/CPU/RAM/disk probes
internal/engines/ollama/   Ollama adapter
internal/engines/llamacpp/ llama-server adapter
internal/engines/vllm/     vLLM adapter
internal/compat/           rule engine + estimators
internal/manifest/         provenance and hashing
internal/residency/        admission + eviction
internal/metrics/          collectors + Prometheus output
web/                       React UI
configs/                   default configs
scripts/                   dev and validation scripts
```

## First code milestone

The first meaningful PR should expose only:

- `GET /api/hardware`
- `GET /api/hardware/live` SSE
- `GET /api/engines`
- `GET /api/models`
- SQLite migration bootstrap
- minimal React page with hardware + engine status

No model start/stop in the first milestone.

## Coding rules

- No control-plane CUDA linking.
- No engine internal patching.
- No mutation before Phase 4.
- No user-facing opaque errors.
- All process starts bind to localhost.
- Reconcile state on every daemon startup.
- Every engine-specific behavior must cite current upstream docs in code comments or commit messages when non-obvious.
