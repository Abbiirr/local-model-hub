// This file builds the exact CLI commands the vLLM adapter runs (P4.4, CONTRACT level). The
// command strings are unit-tested string-exactly (command_test.go) so a future vLLM/docker change
// is caught by a failing test, not a broken real run. NOTHING here executes a command — building
// the argv is separate from running it (the DockerClient seam runs it).
//
// [RE-VERIFY 2026-07-06] Verified against the current vLLM docs:
//   - Docker run (docs.vllm.ai/en/stable/deployment/docker/): the canonical invocation is
//     docker run --runtime nvidia --gpus all \
//     -v ~/.cache/huggingface:/root/.cache/huggingface \
//     --env "HF_TOKEN=$HF_TOKEN" -p 8000:8000 --ipc=host \
//     vllm/vllm-openai:latest --model <MODEL>
//     vLLM listens on container port 8000 by default; engine args (‑‑model, ‑‑max-model-len,
//     ‑‑gpu-memory-utilization, ‑‑dtype) are passed AFTER the image name. We bind the host port
//     to 127.0.0.1:PORT (localhost gate) and name the container localhub-vllm-<instanceID> so
//     stop/reconcile can find it by name.
//   - `vllm serve <model> --host 127.0.0.1 --port <p> --gpu-memory-utilization <u>
//     --max-model-len <n> --dtype auto` is the subprocess (Linux) invocation
//     (docs.vllm.ai online-serving). --gpu-memory-utilization default is 0.9; --max-model-len
//     defaults to the model's max (we set it explicitly from the compat plan). Health is
//     GET /health; readiness cross-check is GET /v1/models.
package vllm

import (
	"fmt"
	"strings"
)

// DefaultImage is the vLLM OpenAI-compatible server image (PLAN.md: already present on the box).
const DefaultImage = "vllm/vllm-openai:latest"

// containerVLLMPort is the port vLLM listens on INSIDE the container (vLLM default).
const containerVLLMPort = 8000

// ContainerName returns the deterministic docker container name for an instance:
// localhub-vllm-<instanceID>. Stop and reconcile find the container by this name.
func ContainerName(instanceID string) string {
	// Docker names allow [a-zA-Z0-9][a-zA-Z0-9_.-]; a uuid is safe, but sanitize defensively.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, instanceID)
	return "localhub-vllm-" + safe
}

// LaunchParams are the model + memory knobs a vLLM start needs (from the compat safetensors plan).
type LaunchParams struct {
	InstanceID  string
	Model       string  // HF repo id or local path passed to --model
	HostPort    int     // host port to bind (127.0.0.1:HostPort → container 8000)
	GPUMemUtil  float64 // --gpu-memory-utilization (0.9 default; 0 => omit, use vLLM default)
	MaxModelLen int     // --max-model-len (tokens); 0 => omit (vLLM picks the model max)
	Dtype       string  // --dtype (auto|float16|bfloat16|...); "" => "auto"
	Image       string  // docker image; "" => DefaultImage
	ExtraArgs   []string
	// ExtraDockerArgs are DOCKER-level args (volume mounts, env, etc.) inserted BEFORE the image
	// name in `docker run` — e.g. `-v K:\hf-cache:/root/.cache/huggingface` and
	// `-e HF_HUB_ENABLE_HF_TRANSFER=0`. Distinct from ExtraArgs, which are vLLM engine flags
	// appended AFTER the image name. Docker-only (ignored for the subprocess mode).
	ExtraDockerArgs []string
}

// DockerRunArgs builds the argv for `docker run` (excluding the leading "docker"). It is the
// exact list the DockerClient.Run executes. Order is fixed and unit-tested string-exactly.
//
//	docker run -d --name localhub-vllm-<id> --runtime nvidia --gpus all --ipc=host \
//	  -p 127.0.0.1:<hostPort>:8000 [extra-docker-args...] <image> --model <model> \
//	  --gpu-memory-utilization <u> --max-model-len <n> --dtype <d> [extra...]
func DockerRunArgs(p LaunchParams) []string {
	image := p.Image
	if image == "" {
		image = DefaultImage
	}
	args := []string{
		"run", "-d",
		"--name", ContainerName(p.InstanceID),
		"--runtime", "nvidia",
		"--gpus", "all",
		"--ipc=host",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", p.HostPort, containerVLLMPort),
	}
	// Docker-level extra args (volume mounts, env) go BEFORE the image name.
	args = append(args, p.ExtraDockerArgs...)
	args = append(args,
		image,
		"--model", p.Model, // docker passes the model as a flag after the image
	)
	args = append(args, engineArgs(p)...)
	return args
}

// DockerStopArgs / DockerRmArgs / DockerPsNameArgs build the container lifecycle commands.
func DockerStopArgs(instanceID string) []string {
	return []string{"stop", ContainerName(instanceID)}
}

func DockerRmArgs(instanceID string) []string {
	return []string{"rm", "-f", ContainerName(instanceID)}
}

// DockerPsNameArgs builds `docker ps --filter name=<name> --format {{.Names}}` to check whether a
// container is still running (reconcile + stop-verify).
func DockerPsNameArgs(instanceID string) []string {
	return []string{"ps", "--filter", "name=" + ContainerName(instanceID), "--format", "{{.Names}}"}
}

// DockerLogsTailArgs builds `docker logs --tail <n> <name>` for the STARTING progress poll.
func DockerLogsTailArgs(instanceID string, tail int) []string {
	if tail <= 0 {
		tail = 40
	}
	return []string{"logs", "--tail", fmt.Sprintf("%d", tail), ContainerName(instanceID)}
}

// SubprocessArgs builds the `vllm serve` argv (excluding the leading "vllm") for the Linux
// subprocess mode. Not runnable on Windows (guarded in the runner).
//
// vllm serve <model> --host 127.0.0.1 --port <p> --gpu-memory-utilization <u>
//
//	--max-model-len <n> --dtype <d> [extra...]
func SubprocessArgs(p LaunchParams) []string {
	// vllm serve places the model POSITIONALLY (not as --model), then the engine flags.
	args := []string{"serve", p.Model, "--host", "127.0.0.1", "--port", fmt.Sprintf("%d", p.HostPort)}
	args = append(args, engineArgs(p)...)
	return args
}

// engineArgs builds the shared vLLM engine flags (--gpu-memory-utilization/--max-model-len/
// --dtype/extra), WITHOUT --model (docker adds --model after the image; subprocess passes the
// model positionally). Order is fixed and unit-tested string-exactly.
func engineArgs(p LaunchParams) []string {
	var args []string
	if p.GPUMemUtil > 0 {
		args = append(args, "--gpu-memory-utilization", trimFloat(p.GPUMemUtil))
	}
	if p.MaxModelLen > 0 {
		args = append(args, "--max-model-len", fmt.Sprintf("%d", p.MaxModelLen))
	}
	dtype := p.Dtype
	if dtype == "" {
		dtype = "auto"
	}
	args = append(args, "--dtype", dtype)
	if len(p.ExtraArgs) > 0 {
		args = append(args, p.ExtraArgs...)
	}
	return args
}

// trimFloat formats a gpu-memory-utilization like 0.9 as "0.9" (no trailing zeros), matching how a
// human would type it — so the unit-tested command string is stable.
func trimFloat(f float64) string {
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", f), "0"), ".")
	if s == "" {
		s = "0"
	}
	return s
}
