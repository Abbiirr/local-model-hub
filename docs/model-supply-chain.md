# Model Supply Chain

The reproducibility/provenance layer. This is a real differentiator: most tools *run*
models; few make revision pinning, hashing, license surfacing, and security warnings a
first-class, always-on part of the download UX.

## Sources supported (MVP)

```
huggingface   -> repo snapshot at a pinned revision (safetensors or GGUF)
gguf_url      -> single .gguf direct download
ollama        -> ollama pull (Ollama owns the artifact; we record the ref + digest)
local_import  -> user points at an existing file/dir on disk
```

## The core rule: pin the revision

Never download "latest." Resolve the HF ref to an **exact commit SHA** and pin it. The HF
Hub API returns the commit for a ref; store that SHA in both `models.revision` and the
manifest. A model "qwen2.5-coder-7b" without a commit is not reproducible; the same name
can point at different bytes next week.

```
resolve(repo, ref="main") -> commit_sha           # via HF Hub API
download(repo, revision=commit_sha)                 # pin, don't float
```

## Manifest (immutable, one per download)

Produced at download time, never mutated. Shape (stored in `manifests`):

```json
{
  "id": "b3f1…",
  "source": "huggingface",
  "repo": "Qwen/Qwen2.5-Coder-7B-Instruct",
  "revision": "e3a1c0f…",                 // exact commit SHA
  "format": "safetensors",
  "quantization": null,
  "license": "apache-2.0",
  "files": [
    { "path": "model-00001-of-00004.safetensors", "sha256": "…", "size_bytes": 4976000000 },
    { "path": "config.json", "sha256": "…", "size_bytes": 812 }
  ],
  "security_warnings": [],
  "downloaded_at": "2026-07-06T10:00:00Z"
}
```

Manifest + `instances.launch_flags_json` together = full reproduction of a working run
(what bytes, at what revision, with what flags). Surface a "reproduce this" export.

## Hashing

- Compute `sha256` per file during/after download; store in the manifest.
- Where HF publishes expected hashes/ETags, verify against them and flag mismatches.
- The primary artifact hash (or the index file hash for sharded models) goes on the
  `models` row for quick integrity checks.

## Security warnings (surface, don't silently block — see ADR-004)

Scan the downloaded fileset and attach warnings to the manifest:

| Condition | Warning |
|---|---|
| Contains `.bin`/`.pt`/`.pth` pickle weights, no safetensors | "Contains pickle-format weights. Pickle can execute code on load. Prefer a safetensors version." |
| `config.json` or repo requires `trust_remote_code` | "This model requires trust_remote_code — arbitrary code runs at load. Review the repo before enabling." |
| No `LICENSE`/license metadata | "No license detected. Check usage rights before deploying." |
| Gated/consent repo | "Gated model — access terms apply." |

Policy stance: **prefer** safetensors and **warn** loudly on pickle/`trust_remote_code`;
do not hard-block by default, because real users legitimately need pickle-only or
`trust_remote_code` models and a hard block just makes them disable safety entirely.
A user-set "strict mode" *can* hard-block — opt-in, not default. Rationale in
[ADR-004](decisions.md#adr-004).

## Disk accounting

- Track `size_bytes` per model; show total disk used and free.
- **Do not** store both safetensors and an auto-converted GGUF by default — that doubles
  disk for every model and conversion is a landmine (ADR-004). Store what the user
  downloaded; conversion is a separate, manual, advanced action.

## Download UX requirements

- Show resolved commit SHA before downloading (so the user sees what they're pinning).
- Show the compatibility verdict **before** committing to a large download — the whole
  point of `gguf-parser-go`'s remote metadata read (see
  [compatibility-engine.md](compatibility-engine.md)).
- Resumable downloads; verify hashes on completion; mark partial/failed clearly.
