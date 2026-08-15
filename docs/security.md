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
3. **Optional bearer token — enforced (shipped).** For LAN/headless setups, set a single
   admin token in `server.admin_token`. When set, `authMiddleware`
   (`internal/api/middleware.go`) requires `Authorization: Bearer <admin_token>` on **every
   `/api/*` and `/v1/*` request**, using a constant-time compare (`crypto/subtle`). Two paths
   are exempt by design: `GET /api/health` (so liveness probes still work) and the static UI
   assets on `/` (they are useless without API access, and a browser cannot inject the header —
   the token's audience is headless/API clients). When the token is null (default), the trust
   model is the loopback bind + Tailscale identity above. **Caveat:** the token gates admin
   *and* proxy inference endpoints uniformly; there is no per-endpoint scoping, and static UI
   assets are always reachable. Do not treat the exempt static assets as a leak of control —
   they cannot drive the API without the token.

4. **Anthropic adapter header alias — shipped with ADR-010.** On the experimental
   `/v1/messages` and `/v1/messages/count_tokens` routes only, `x-api-key:
   <admin_token>` is accepted as an alias for `Authorization: Bearer <admin_token>` so
   standard Anthropic clients and Claude Code can use the same single LocalHub credential.
   This is compatibility syntax, not API-key management: LocalHub issues no Anthropic key,
   creates no per-user keys, and never forwards either credential to an engine. If both
   headers are present they must match the configured token. Comparison remains constant-
   time. Until this adapter is implemented, the shipped middleware behavior remains Bearer-
   only as described above.

### Two operating modes (owner directive, 2026-07-25)

The owner's intended end state is **two modes**: LAN reachable without a password now, and
Tailscale access gated by a token/password later.

**Mode A — LAN-open, passwordless (ACTIVE on the reference box).**
```yaml
server:
  bind: "0.0.0.0"        # publishes on every interface
  allow_nonlocal: true   # required, else Validate() hard-fails
  admin_token: null      # no credential
```
Other hosts reach the API and dashboard on **port 39876** (`http://<host-ip>:39876`). Engine
ports (40100+) stay loopback-only; all traffic goes through 39876 so it passes admission and
in-flight accounting. **Every caller that can reach the port has full admin rights** — start and
stop GPU processes, trigger model downloads, read inventory. Only acceptable on a network you
fully trust. Note `0.0.0.0` publishes on *all* interfaces, which on this box includes two
ZeroTier overlays (10.147.19.x, 172.27.25.x) and therefore remote overlay peers, not just the
Ethernet LAN.

**Mode B — Tailscale with token enforced (PLANNED, not built).**
```yaml
server:
  bind: "127.0.0.1"           # Tailscale Serve is the only proxy
  allow_nonlocal: false
  admin_token: "<random>"     # Authorization: Bearer <token> on every /api/* and /v1/*
  trust_tailscale_headers: true
```

> **Known limitation — the two modes cannot coexist today.** `admin_token` is a *single global*
> switch: `authMiddleware` applies it to every `/api/*` and `/v1/*` request regardless of the
> request's origin. There is no way to express "no credential from the LAN, credential required
> over Tailscale" with current config — enabling the token for Tailscale would also lock out
> passwordless LAN clients. Delivering the owner's two-mode design requires **origin-aware
> auth**: classify each request (loopback / LAN / tailnet, the last via the Tailscale identity
> headers `trust_tailscale_headers` already governs) and apply a per-origin policy. That is new
> work, not a config change. Until it exists, pick one mode at a time.

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
- Anthropic `tool_use` is data returned to the client. LocalHub must not execute a tool
  merely because a model emitted a tool block; Claude Code or the calling client owns tool
  approval and execution.
- Codex Responses namespace translation is naming metadata only. LocalHub may expose a
  namespaced client tool to the local model as a flat function and restore the namespace on
  the returned call, but it never executes the function; Codex and its MCP approval policy
  remain the authority.

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
4. **Credential confusion at the Anthropic compatibility edge** (treating `x-api-key` as
   an upstream Anthropic credential or forwarding it to an engine) → mitigated by treating
   it only as an alias for LocalHub's configured admin token, consuming it at the edge, and
   preserving the localhost/Tailscale exposure defaults.

Out of scope: multi-tenant isolation, adversarial co-tenants, supply-chain attacks on the
engines themselves. Say so; don't imply guarantees you don't provide.
