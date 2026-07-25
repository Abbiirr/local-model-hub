# 19 — Release Runbook

## Pre-release

1. Re-run `docs/18-source-validation-matrix.md`.
2. Run all unit and adapter tests.
3. Run phase gates for the release scope.
4. Run clean install on a fresh machine/container.
5. Confirm default bind is `127.0.0.1`.
6. Confirm Tailscale Serve instructions still work.
7. Confirm no dangerous vLLM dev endpoints are proxied.
8. Confirm all downloads create manifests and reproduce exports.
9. Update changelog and known limitations.
10. Tag release only after all non-waivable gates pass.

## Release artifacts

- Binary or container image.
- Web UI bundle.
- Example config.
- Checksums.
- Changelog.
- Known limitations.
- Source-validation date.

## Rollback plan

For a local tool, rollback must be simple:

1. Stop daemon.
2. Restore previous binary.
3. Keep SQLite DB backup before migration.
4. Do not delete model files automatically.
5. Log any orphaned engine process and provide cleanup command.
