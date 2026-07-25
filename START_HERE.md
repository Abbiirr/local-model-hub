# START HERE — LocalHub Compass Project Handoff

This package is the project-start bundle for building LocalHub Compass from zero to MVP and optional proxy release.

## Read order

1. `docs/00-validation-verdict.md`
2. `docs/product-brief.md`
3. `docs/decisions.md`
4. `docs/14-closing-gates.md`
5. `docs/16-implementation-playbook.md`
6. `project-management/task-breakdown.csv`
7. The phase prompt under `prompts/` for the phase being built.

## Non-negotiable scope

LocalHub Compass is a single-box tool for local LLM operators. Its core job is:

> inventory local models, predict what runs on this hardware, pin/download artifacts reproducibly, run/stop models conservatively, manage coarse residency, and show live GPU/model/queue status from phone over Tailscale.

Do not build a GPUStack clone, Kubernetes scheduler, chat UI, LiteLLM replacement, training system, or multi-tenant SaaS platform.

## Current validated project decision

Proceed only through Phase 0 first. Phase 0 must prove that existing tools do not already solve the user's actual workflow. The valid outcomes are:

- standalone build,
- upstream contribution,
- stop.

Do not skip that gate.
