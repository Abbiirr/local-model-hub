# Remote llama.cpp models through LocalHub

LocalHub can expose an already-running, single-model llama.cpp server on the same
LAN or tailnet through LocalHub's existing OpenAI-compatible surface.

```yaml
engines:
  llamacpp:
    enabled: true
    remote_models:
      - id: "kat"
        base_url: "http://192.168.0.157:8080"
        upstream_model: "/home/abir/models/kat-coder-v2.5/KAT-Coder-V2.5-Dev-MTP-MXFP4_MOE.gguf"
```

Downstream clients configure only LocalHub:

```text
base_url = http://<localhub-host>:39876/v1
model = kat
```

The `kat` API model routes to KAT-Coder V2.5. Downstream clients use this KAT
identity while LocalHub keeps the remote server's filesystem model identifier
private.

LocalHub rewrites the stable alias to the remote server's exact model identifier
and relays through the same `/v1/chat/completions`, `/v1/completions`,
`/v1/responses`, `/v1/messages`, and async-job resolution paths used for locally
resident models.

Before advertising the alias in `GET /v1/models` or routing inference, LocalHub
verifies:

1. `GET <base_url>/health` returns `{"status":"ok"}`.
2. `GET <base_url>/v1/models` advertises the exact configured `upstream_model`.

An unreachable server or model-identity mismatch fails closed with a
human-readable upstream error. LocalHub does not start, stop, recover, admit, or
account the remote machine's GPU; lifecycle ownership stays on that machine.

This is fixed llama.cpp engine configuration, not a general provider gateway:
clients cannot supply an arbitrary target URL, and the configured URL cannot
contain credentials, paths, queries, or fragments.
