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
