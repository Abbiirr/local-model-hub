# Security

Guiding principle: **security posture must not defeat the 5-minute install.** The
original plan reached for Tailscale-required + rootless + gVisor by default. For a FOSS
tool aiming at broad adoption, mandatory heavyweight isolation is friction that pushes
users to just run the raw engines instead. So: strong *defaults*, but each layer is
optional and degrades gracefully.

## Network exposure

- **Bind to `127.0.0.1` by default.** LocalHub controls processes and can free/allocate
  GPU memory; it should not be on `0.0.0.0` out of the box.
- **Remote access = Tailscale, recommended not required.**
  - Tailscale **Serve** proxies a localhost service to your tailnet with ACLs applied,
    and can pass identity headers to the backend. Good default for phone access.
  - If the control plane is **Go**, `tsnet` is attractive: embed Tailscale directly in
    the binary and expose a first-class tailnet service without binding publicly.
  - LAN-only users can bind to a LAN IP behind their own firewall — supported, with a
    warning that no auth layer is assumed there.
- **Never** put LocalHub directly on the public internet without a reverse proxy + auth.
  Say this loudly in docs.

## Identity / auth

MVP, layered:

1. **localhost trust** — a request on `127.0.0.1` is the machine owner. Fine for the
   single-user default.
2. **Tailscale identity** — when served over Tailscale, trust the identity headers
   Tailscale injects (only when LocalHub itself listens on localhost and Tailscale is the
   proxy; do not trust such headers on an arbitrary bind).
3. **Optional bearer token** — for LAN/headless setups, a single admin token in config.

Do not build OAuth/multi-tenant/RBAC in MVP — that's the enterprise path this product
explicitly isn't (see [product-brief.md](product-brief.md#non-goals)).

## The dangerous endpoints

Some engine controls can disrupt or free memory out from under running work. Treat as
admin-only, localhost-bound, never client-facing:

- vLLM `/sleep`, `/wake_up`, `/collective_rpc`, `/reset_prefix_cache` — these require
  `VLLM_SERVER_DEV_MODE=1` and vLLM's docs say they must not be exposed to users. If
  LocalHub enables sleep mode for an instance, it keeps these bound to localhost and
  behind the admin auth layer; the inference proxy never forwards them.
- LocalHub's own instance stop/evict and `POST /api/instances` — admin-only.

## Model artifact safety (pickle / trust_remote_code)

- **Prefer safetensors.** HF describes safetensors as a safe tensor storage format, in
  contrast to pickle which can execute arbitrary code on load.
- **Warn, don't hard-block by default.** Reality: many popular models still ship
  pickle-format weights or require `trust_remote_code`. A hard block by default makes
  users disable safety wholesale. So default = loud warning attached to the manifest and
  shown before load; **opt-in "strict mode"** = actually block pickle/`trust_remote_code`.
  Full rationale in [ADR-004](decisions.md#adr-004).
- Warnings are computed at download time and stored in the manifest (see
  [model-supply-chain.md](model-supply-chain.md)).

## Process isolation (proportional, not maximal)

- **Rootless / non-root user:** yes, run the control plane and engines as a non-root
  user by default. Cheap, real benefit.
- **Containers:** support running engines in containers (many users already do for vLLM);
  don't *require* it.
- **gVisor for untrusted conversion:** the original plan put gVisor in the core. Since
  auto-conversion is **cut from core** (ADR-004), the main motivation for gVisor goes
  with it. If/when a manual conversion feature runs untrusted `trust_remote_code` during
  HF→GGUF conversion, *then* sandbox that specific subprocess (gVisor, or a throwaway
  container). Do not impose a gVisor dependency on the whole product for a feature that
  isn't in MVP.

## Threat model (state it plainly)

LocalHub's realistic threats, in order:

1. **Malicious model artifact** executing code at load (pickle/`trust_remote_code`) →
   mitigated by safetensors preference + warnings + optional strict mode + non-root.
2. **Accidental public exposure** of an admin-capable service → mitigated by
   localhost-default bind + Tailscale + explicit warnings.
3. **A resident model's dev endpoints** (vLLM sleep) being reachable → mitigated by
   localhost binding + not proxying them.

Out of scope: multi-tenant isolation, adversarial co-tenants, supply-chain attacks on the
engines themselves. Say so; don't imply guarantees you don't provide.
