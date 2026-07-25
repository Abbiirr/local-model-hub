# 22 — Engine Version Policy

LocalHub supports three fast-moving engines. Version drift is a product risk, not a detail.

## Policy

- Each adapter records engine binary/container version.
- Each compatibility verdict records estimator version and engine recommendation basis.
- Each launched instance records exact launch flags.
- For vLLM, prefer containerized execution or a pinned Python environment.
- For llama.cpp, record binary path and `llama-server --version`.
- For Ollama, record daemon version and model digest.

## Support tiers

| Tier | Meaning |
|---|---|
| Known-good | Version tested in current release gate. |
| Detected-untested | Version detected but not in gate matrix; warn. |
| Unsupported | Version fails adapter contract or known incompatible. |

## Release requirement

Every release must document the known-good matrix:

- OS,
- GPU,
- driver,
- CUDA/ROCm,
- Ollama version,
- llama.cpp server version,
- vLLM version,
- gguf-parser-go version.
