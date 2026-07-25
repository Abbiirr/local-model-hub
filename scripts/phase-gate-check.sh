#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 GATE_ID" >&2
  exit 2
fi

GATE="$1"
echo "Checking gate: $GATE"
echo "Read docs/14-closing-gates.md and attach evidence before marking PASS."
case "$GATE" in
  G0) test -f research/phase-0-friction-log.md || { echo "missing research/phase-0-friction-log.md"; exit 1; } ;;
  G1) echo "verify /api/hardware, /api/engines, /api/models, phone dashboard" ;;
  G2) echo "verify 20-model matrix and zero false-positive runs" ;;
  G3) echo "verify manifests, hashes, warnings, reproduce export" ;;
  G4) echo "verify start/stop and VRAM reclaimed for all engines" ;;
  G5) echo "verify load-storm no OOM/no active kill/no crash" ;;
  G6) echo "verify thin proxy streaming and 503 blocked_reason" ;;
  *) echo "unknown gate"; exit 2 ;;
esac
