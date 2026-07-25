#!/usr/bin/env bash
set -euo pipefail

echo "== System =="
uname -a || true

echo "== NVIDIA =="
if command -v nvidia-smi >/dev/null 2>&1; then
  nvidia-smi
  nvidia-smi --query-gpu=name,memory.total,memory.free,driver_version,temperature.gpu,power.draw,utilization.gpu --format=csv || true
else
  echo "nvidia-smi not found"
fi

echo "== Engines =="
command -v ollama >/dev/null 2>&1 && ollama --version || echo "ollama not found"
command -v llama-server >/dev/null 2>&1 && llama-server --version || echo "llama-server not found"
command -v vllm >/dev/null 2>&1 && vllm --version || echo "vllm not found"
command -v tailscale >/dev/null 2>&1 && tailscale status || echo "tailscale not found"

echo "== Ports =="
ss -ltnp 2>/dev/null | grep -E '11434|39876|401|402|403' || true
