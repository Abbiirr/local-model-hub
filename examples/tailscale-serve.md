# Tailscale Serve Example

Default LocalHub should bind to localhost:

```bash
localhub --config ./config.yaml
# serving http://127.0.0.1:39876
```

Expose it to your tailnet:

```bash
tailscale serve 39876
```

Then open the printed HTTPS tailnet URL on your phone.

Security rules:

- Use Tailscale Serve, not Funnel, for private phone access.
- Trust Tailscale identity headers only when LocalHub is bound to localhost and Serve is the proxy.
- Do not expose LocalHub directly to public internet.

## Troubleshooting: Tailscale backend wedged in `NoState`

On Windows the Tailscale backend can wedge in `NoState`, and `tailscale serve` then fails to
establish the proxy. Recovering needs an **elevated shell** — from an admin PowerShell:

```powershell
Restart-Service Tailscale -Force
```

Then re-run `tailscale serve 39876` and open the printed ts.net URL. This restart requires
administrator rights, so an unprivileged/automated agent cannot perform it; it is a manual step.
(Observed on the reference box during P1.6 phone-access validation.)
