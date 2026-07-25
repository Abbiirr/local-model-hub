# 20 — UI / UX Spec

## Navigation

MVP screens:

1. Dashboard
2. Models
3. Compatibility Preview
4. Downloads / Manifests
5. Running Instances
6. Queue
7. Settings / Tailscale

## Dashboard

Must show:

- GPU memory used/free/total.
- GPU utilization, temperature, power.
- CPU/RAM/disk.
- Running engines and health.
- Running model instances.
- Queue length and blocked reasons.
- Phone connection status / Tailscale hint.

## Model table

Columns:

- name,
- source,
- format,
- quantization,
- size,
- local path/repo,
- manifest status,
- recommended engine,
- compatibility status,
- reason.

## Compatibility verdict UX

Every verdict must display:

- `runs`, `runs_degraded`, `wont_run`, or `unknown`,
- recommended engine,
- estimated VRAM,
- requested context,
- max context that fits,
- generated run flags,
- warnings,
- human-readable reason.

Do not show a green `runs` badge without the reason and margin.

## Dangerous actions

Stop, evict, delete files, force-evict active generation, and loading pickle/trust_remote_code models require an explicit confirmation dialog.

## Mobile requirements

Phone dashboard must be usable at 390px width:

- GPU card first.
- Running models second.
- Queue third.
- Actions hidden behind menus.
- Logs collapsed by default.
