# 15 — Test Strategy

## Test layers

1. **Unit tests:** parsers, estimators, manifests, queue/victim ordering, state machine.
2. **Adapter contract tests:** one suite per engine using fake HTTP servers and process stubs.
3. **Hardware integration tests:** real GPU, real engines, small models.
4. **Failure tests:** OOM prevention, port conflict, process crash, daemon restart, partial download, network failure.
5. **Release acceptance tests:** phase-gate matrix in `project-management/acceptance-test-matrix.csv`.

## Critical correctness tests

### Compatibility false-positive test

For each validation model:

1. record hardware snapshot,
2. compute verdict,
3. if verdict is `runs`, launch with generated flags,
4. run a short prompt at requested context,
5. record observed VRAM and result.

A false-positive `runs` that OOMs is a blocking bug. False negatives are acceptable early if conservative and explained.

### Load-storm test

1. Pick six models whose combined memory exceeds GPU capacity.
2. Pin one model.
3. Start one long streaming request.
4. Request remaining models with mixed priorities.
5. Assert: no active stream killed, pinned model remains, idle/expired models evicted first, blocked entries have reasons, process does not crash.

### Manifest reproducibility test

For each download/import:

1. export reproduce bundle,
2. delete inventory row but keep files,
3. re-import from manifest,
4. verify hashes,
5. reproduce launch command.

### Tailscale exposure test

1. LocalHub binds to `127.0.0.1` by default.
2. Browser on same machine works.
3. Phone over Tailscale Serve works.
4. LAN/public direct access fails unless explicitly configured.
5. Identity headers are trusted only when LocalHub is behind Serve and bound to localhost.

## Test model set

Minimum validation set:

- 5 GGUF models: small, medium, high context, Q4, Q8.
- 5 safetensors models: fp16/bf16, AWQ/GPTQ if available, GQA + MHA.
- 3 models expected not to fit on 16 GB VRAM.
- 3 models expected to fit only at reduced context.
- 2 Ollama registry models.
- 2 local imports with incomplete metadata.

Record the exact repo/file/revision for every test model.
