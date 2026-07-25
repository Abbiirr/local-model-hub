# Ubuntu and SSH readiness

LocalHub is a single Go control-plane binary. Ubuntu is the preferred native host for
vLLM; Windows uses Docker/WSL2 because vLLM is not natively supported there.

## Build and run on Ubuntu

Install Go 1.23 or newer from [go.dev/dl](https://go.dev/dl/) before building.

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl git build-essential
go version
git clone <repo-url> local-model-hub && cd local-model-hub
go build -o localhub ./cmd/localhub
sudo install -m 0755 localhub /usr/local/bin/localhub
sudo useradd --system --home-dir /var/lib/localhub --create-home --shell /usr/sbin/nologin localhub
sudo install -d -o root -g localhub -m 0750 /etc/localhub
sudo install -m 0644 examples/config.example.yaml /etc/localhub/config.yaml
sudo install -m 0644 examples/localhub.service /etc/systemd/system/localhub.service
sudo systemctl daemon-reload && sudo systemctl enable --now localhub
```

Install the NVIDIA driver/CUDA runtime and vLLM/Ollama/llama.cpp separately; the control
plane only supervises external engines. Run `scripts/nvidia-preflight.sh` before enabling
GPU engines. Keep LocalHub bound to `127.0.0.1` and use Tailscale Serve for phone access.

For models that require vLLM runtime switches, configure `engines.vllm.extra_args` in
`/etc/localhub/config.yaml`; LocalHub passes these after the model argument in both Docker
and native Linux subprocess mode. They are engine flags, not Docker flags.

## SSH access

For a one-off admin session, forward the loopback API to the client:

```bash
ssh -N -L 39876:127.0.0.1:39876 user@ubuntu-host
```

Then open `http://127.0.0.1:39876` locally. SSH forwarding does not change LocalHub's bind
or authentication boundary. For persistent phone access, prefer Tailscale Serve as described
in [security.md](security.md) and [tailscale-serve.md](../examples/tailscale-serve.md).

Cross-build verification from another OS is `GOOS=linux GOARCH=amd64 go build ./...`.
