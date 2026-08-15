# LocalHub Compass

**A single-box control plane for local LLM operations.** It tells you which models can
actually run on your exact hardware, through which engine, at which version and flags —
then safely loads/unloads them, monitors VRAM, and lets you drive the machine from your
phone over Tailscale.

> LocalHub Compass tells you exactly which local models can run on your current GPU,
> through which engine, with which version and flags, and safely manages coarse
> load/unload while giving you phone-accessible GPU and queue status over Tailscale.

This is **not** a GPUStack clone, a Kubernetes scheduler, a chat UI, or an AI gateway.
It is the thin, honest layer that those tools skip: pre-flight compatibility, model
supply-chain/reproducibility, and coarse single-box residency across supervised
**llama.cpp + vLLM** runtimes.

---

## Why this exists

The local-model tooling landscape is crowded but bifurcated:

- **Cluster-scale** tools (GPUStack, KubeAI, Xinference distributed) assume many nodes,
  Kubernetes, or a fleet. Heavy for one workstation.
- **Single-engine** tools (Ollama, LM Studio) hide everything and only manage their own
  format. You can't see "will this HF safetensors repo run on my 4060 Ti under vLLM?"
- **Swap-only** tools (llama-swap) hot-swap processes well but have no inventory,
  compatibility model, or supply-chain story.

Nobody does the **single-box, multi-engine, pre-flight-compatibility + supply-chain +
coarse-residency + phone-ops** bundle well. That is the entire wedge. See
[`docs/product-brief.md`](docs/product-brief.md) for the honest competitive delta.

## What it does (MVP)

```
Inventory → Compatibility → Download/Manifest → Run/Stop → Coarse queue → Monitor → Phone control
```

- **Inventory:** one normalized table of local GGUF files and HF safetensors/AWQ
  directories, with format, quant, revision, hash, size, and license.
- **Compatibility:** for each model + your hardware, answers *runs / runs-degraded /
  won't-run*, recommended engine, estimated VRAM, realistic context length, and the
  exact launch flags. Estimation reuses `gguf-parser-go`; it does not reinvent it.
- **Supply-chain:** every download or local import produces an immutable manifest (source,
  HF revision commit, file hashes, license, and security warnings). Reproducible by
  construction.
- **Coarse residency:** admission control + idle/TTL/priority eviction. Never a
  cross-engine VRAM preemption scheduler — that is explicitly out of scope and
  [explained here](docs/decisions.md#adr-001).
- **Inference proxy + async jobs:** a thin OpenAI-compatible proxy (`/v1/chat/completions`,
  `/v1/completions`, `/v1/models`) routes by model name through admission to the right engine
  and streams tokens back byte-for-byte; a durable async-jobs API (`/api/jobs`) submits an
  inference request, returns an id immediately, and survives a daemon restart. A JSON-RPC 2.0
  surface mirrors the control core.
- **Monitoring + phone:** live GPU/engine/queue metrics over SSE and WebSocket, reachable from
  your phone via Tailscale Serve.

## What it explicitly does NOT do

See [`docs/product-brief.md#non-goals`](docs/product-brief.md#non-goals). Short version:
no GPUStack clone, no k8s, no live cross-engine VRAM preemption, no auto
Safetensors→GGUF conversion in core, no Open WebUI replacement, no LiteLLM replacement.

## Status

**v0.1.0 (unreleased) — Phases 0–6 are implemented and live-verified on the reference box;
C1 close-out is in progress.** This is a working Go daemon with a React dashboard, supervised
llama.cpp/vLLM adapters, a compatibility engine, supply-chain manifests, coarse residency,
async jobs, JSON-RPC 2.0, and a thin OpenAI proxy — not a docs bundle. It is not "done":
phone-over-Tailscale still needs a manual user check and release gates are still being stamped.
The proxy's blocked-admission latency gate was resolved at 1.36–1.54 s across eight live
trials. See
[`CHANGELOG.md`](CHANGELOG.md) and [`KNOWN-LIMITATIONS.md`](KNOWN-LIMITATIONS.md) for the
honest scope, and [`docs/roadmap.md`](docs/roadmap.md) for the phase gates.

Reference validation box: RTX 4060 Ti 16 GB (CC 8.9), driver 576.40, 16 GB RAM, Windows 10.
Engines: llama.cpp (CUDA fork + winget Vulkan build), vLLM via Docker (WSL2).

## Quickstart

The daemon embeds the built frontend, so the frontend must be built before `go build`.
Install a supervised `llama-server` for GGUF models. vLLM requires Docker/WSL2 on Windows
and serves Safetensors/AWQ models. Ollama artifacts are not a runtime input for new
deployments; import a standalone GGUF or re-download the model instead.

```sh
# build the UI (Node on PATH), then the daemon
cd web && npm ci && npm run build && cd ..
go build ./...
./localhub            # serves on http://127.0.0.1:39876 by default
```

On Windows, `scripts/dev.ps1` wraps the build/check/run steps. Configuration is optional —
every value in [`configs/config.example.yaml`](configs/config.example.yaml) is also the
built-in default, so the daemon runs with no config file. Copy it to `configs/config.yaml` to
override (model dirs, engine paths, port, `admin_token`, residency/queue tuning, jobs worker).

To relaunch the tray or daemon from the Windows Start Menu instead of a terminal, run
`scripts/install-start-menu.ps1` after building both binaries. It creates a "LocalHub" Start
Menu folder with "LocalHub Tray" and "LocalHub Daemon" shortcuts. It's separate from the
tray's own "Start with Windows" menu toggle (an HKCU `Run` key entry) — this script only adds
an on-demand launch point. Re-running it overwrites the shortcuts in place (no duplicates).
Flags: `-RepoRoot <path>` (defaults to the repo root), `-Startup` (also auto-launch the tray
at login via the per-user Startup folder — off by default), `-Uninstall` (remove everything
this script created).

## API surfaces

All bind to `127.0.0.1` by default (see [Hardware safeguards](#hardware-safeguards--auth)).
Full spec in [`docs/api-spec.md`](docs/api-spec.md).

- **REST** — hardware, engines, models/inventory, compatibility verdicts, supply-chain
  downloads/manifests/reproduce, instances (start/stop/list), residency queue, async jobs.
- **SSE** — `GET /api/hardware/live` (hardware snapshot ~1 Hz).
- **WebSocket** — `GET /api/ws` (server-push snapshots + change signals + `jobs.updated`
  events; also accepts inbound JSON-RPC frames).
- **JSON-RPC 2.0** — `POST /api/rpc` + over `/api/ws`; mirrors the control core.
- **OpenAI proxy** — `GET /v1/models`, `POST /v1/chat/completions`, `POST /v1/completions`.

## Hardware safeguards + auth

- **Binds `127.0.0.1` by default.** Non-loopback binds hard-fail unless `allow_nonlocal` is
  explicitly set. Remote access is via Tailscale Serve, not a public bind.
- **Optional bearer token, enforced.** Set `server.admin_token` and every `/api/*` + `/v1/*`
  request must carry `Authorization: Bearer <token>` (constant-time compare; `GET /api/health`
  and static UI assets exempt). See [`docs/security.md`](docs/security.md).
- **Load decisions are conservative and re-measured.** Admission reads live free VRAM via the
  probe; `stop`/`evict` verify VRAM actually freed (probe delta, never a trusted HTTP 200);
  an active generation is not killed by default. Every verdict/refusal carries a human
  `reason`.

## Local git note

This repository is already git-initialized locally. Any git history changes (commits, pushes)
are performed by the user, not by automated agents — the working tree is prepared, the commit
is yours to make.

## Docs map

| Doc | What it covers |
|---|---|
| [product-brief.md](docs/product-brief.md) | Target user, wedge, non-goals, competitor delta |
| [architecture.md](docs/architecture.md) | Components, process model, data flow |
| [data-model.md](docs/data-model.md) | SQLite schema (DDL) |
| [compatibility-engine.md](docs/compatibility-engine.md) | VRAM estimation + rule engine (the differentiator) |
| [engine-adapters.md](docs/engine-adapters.md) | Adapter contract + llama.cpp/vLLM specifics |
| [residency-scheduler.md](docs/residency-scheduler.md) | Coarse admission control + eviction |
| [model-supply-chain.md](docs/model-supply-chain.md) | Download + manifest + reproducibility |
| [api-spec.md](docs/api-spec.md) | Control API (REST/SSE/WS/JSON-RPC) + thin OpenAI proxy + async jobs |
| [security.md](docs/security.md) | Tailscale, pickle policy, binding, identity, enforced bearer token |
| [roadmap.md](docs/roadmap.md) | Phase 0–6 with acceptance criteria; cluster planning is in [cluster-plan.md](docs/cluster-plan.md) |
| [decisions.md](docs/decisions.md) | ADRs — the load-bearing "why" |
| [CHANGELOG.md](CHANGELOG.md) | v0.1.0 shipped surface, per phase |
| [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md) | Honest limits (RAM ceiling, TQ3 fallback, proxy-block latency, …) |

Agents (Claude Code etc.): read [`CLAUDE.md`](CLAUDE.md) first.

## License

Intended: **Apache-2.0** (permissive, compatible with wrapping `gguf-parser-go` which is
Apache-2.0). Confirm before first public commit.
