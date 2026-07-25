# Full Backlog

The canonical task list is `task-breakdown.csv`. Use this file for narrative planning.

## Milestone 0 — Field validation

Close Phase 0 before coding. Produce the friction log and decision.

## Milestone 1 — Read-only product slice

A user can install LocalHub, open the dashboard locally or through Tailscale, and see hardware, engines, and model inventory. No model mutation.

## Milestone 2 — Compatibility preview

A user can ask whether a local or remote model fits before downloading or running it.

## Milestone 3 — Reproducible model supply chain

A user can download/import a model and get an immutable manifest with warnings and hashes.

## Milestone 4 — Controlled model lifecycle

A user can start/stop a model through Ollama, llama.cpp, or vLLM and see real memory effects.

## Milestone 5 — Safe coarse residency

A user can request more models than fit; LocalHub evicts or queues safely, never killing active streams by default.

## Milestone 6 — Optional thin proxy

One OpenAI-compatible local URL can reach any managed local model, but no gateway feature creep.
