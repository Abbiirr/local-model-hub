package vllm

import (
	"strings"
	"testing"
)

// TestDockerRunArgs_StringExact pins the exact docker run argv against the vLLM docs invocation
// (RE-VERIFY 2026-07-06). A change to any flag/order fails here — the intended tripwire.
func TestDockerRunArgs_StringExact(t *testing.T) {
	got := DockerRunArgs(LaunchParams{
		InstanceID: "abc-123", Model: "Qwen/Qwen3-0.6B", HostPort: 40201,
		GPUMemUtil: 0.9, MaxModelLen: 8192, Dtype: "auto",
	})
	want := []string{
		"run", "-d",
		"--name", "localhub-vllm-abc-123",
		"--gpus", "all",
		"--ipc=host",
		"-p", "127.0.0.1:40201:8000",
		"vllm/vllm-openai:latest",
		"--model", "Qwen/Qwen3-0.6B",
		"--gpu-memory-utilization", "0.9",
		"--max-model-len", "8192",
		"--dtype", "auto",
	}
	assertArgs(t, "docker run", got, want)
	// The full command a human would run, for the audit trail.
	full := "docker " + strings.Join(got, " ")
	const wantFull = "docker run -d --name localhub-vllm-abc-123 --gpus all --ipc=host -p 127.0.0.1:40201:8000 vllm/vllm-openai:latest --model Qwen/Qwen3-0.6B --gpu-memory-utilization 0.9 --max-model-len 8192 --dtype auto"
	if full != wantFull {
		t.Errorf("full command mismatch:\n got: %s\nwant: %s", full, wantFull)
	}
}

func TestDockerRunArgs_OmitsUnsetKnobs(t *testing.T) {
	got := DockerRunArgs(LaunchParams{InstanceID: "x", Model: "m", HostPort: 8000})
	// No --gpu-memory-utilization, no --max-model-len; --dtype defaults to auto.
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--gpu-memory-utilization") {
		t.Error("gpu-memory-utilization must be omitted when unset")
	}
	if strings.Contains(joined, "--max-model-len") {
		t.Error("max-model-len must be omitted when unset")
	}
	if !strings.Contains(joined, "--dtype auto") {
		t.Error("dtype should default to auto")
	}
}

// TestDockerRunArgs_ExtraDockerArgsBeforeImage verifies that docker-level extra args (volume
// mounts, env) are inserted BEFORE the image name, while vLLM engine flags stay AFTER it. This is
// the P4.5 HF-cache-off-C: path: `-v K:\hf-cache:/root/.cache/huggingface -e HF_HUB_ENABLE_HF_TRANSFER=0`.
func TestDockerRunArgs_ExtraDockerArgsBeforeImage(t *testing.T) {
	got := DockerRunArgs(LaunchParams{
		InstanceID: "abc-123", Model: "Qwen/Qwen3-0.6B", HostPort: 40201,
		GPUMemUtil: 0.9, MaxModelLen: 2048, Dtype: "auto",
		ExtraDockerArgs: []string{"-v", `K:\hf-cache-p45:/root/.cache/huggingface`, "-e", "HF_HUB_ENABLE_HF_TRANSFER=0"},
	})
	want := []string{
		"run", "-d",
		"--name", "localhub-vllm-abc-123",
		"--gpus", "all",
		"--ipc=host",
		"-p", "127.0.0.1:40201:8000",
		"-v", `K:\hf-cache-p45:/root/.cache/huggingface`,
		"-e", "HF_HUB_ENABLE_HF_TRANSFER=0",
		"vllm/vllm-openai:latest",
		"--model", "Qwen/Qwen3-0.6B",
		"--gpu-memory-utilization", "0.9",
		"--max-model-len", "2048",
		"--dtype", "auto",
	}
	assertArgs(t, "docker run with extra docker args", got, want)
	// The docker-level args must appear before the image, and engine flags after it.
	joined := strings.Join(got, " ")
	vIdx := strings.Index(joined, "-v ")
	imgIdx := strings.Index(joined, "vllm/vllm-openai")
	engIdx := strings.Index(joined, "--gpu-memory-utilization")
	if !(vIdx < imgIdx && imgIdx < engIdx) {
		t.Errorf("ordering wrong: -v@%d image@%d engine@%d in %q", vIdx, imgIdx, engIdx, joined)
	}
}

func TestSubprocessArgs_StringExact(t *testing.T) {
	got := SubprocessArgs(LaunchParams{
		Model: "Qwen/Qwen3-0.6B", HostPort: 40201, GPUMemUtil: 0.9, MaxModelLen: 8192, Dtype: "bfloat16",
	})
	want := []string{
		"serve", "Qwen/Qwen3-0.6B",
		"--host", "127.0.0.1", "--port", "40201",
		"--gpu-memory-utilization", "0.9",
		"--max-model-len", "8192",
		"--dtype", "bfloat16",
	}
	assertArgs(t, "vllm serve", got, want)
}

func TestContainerName(t *testing.T) {
	if got := ContainerName("abc-123"); got != "localhub-vllm-abc-123" {
		t.Errorf("ContainerName: got %q", got)
	}
	// Illegal chars are sanitized to '-'.
	if got := ContainerName("a/b:c"); got != "localhub-vllm-a-b-c" {
		t.Errorf("ContainerName sanitize: got %q", got)
	}
}

func TestDockerLifecycleArgs(t *testing.T) {
	if got := strings.Join(DockerStopArgs("id"), " "); got != "stop localhub-vllm-id" {
		t.Errorf("stop args: %q", got)
	}
	if got := strings.Join(DockerRmArgs("id"), " "); got != "rm -f localhub-vllm-id" {
		t.Errorf("rm args: %q", got)
	}
	if got := strings.Join(DockerPsNameArgs("id"), " "); got != "ps --filter name=localhub-vllm-id --format {{.Names}}" {
		t.Errorf("ps args: %q", got)
	}
}

func TestTrimFloat(t *testing.T) {
	cases := map[float64]string{0.9: "0.9", 0.85: "0.85", 1.0: "1", 0.5: "0.5", 0.95: "0.95"}
	for in, want := range cases {
		if got := trimFloat(in); got != want {
			t.Errorf("trimFloat(%v): got %q want %q", in, got, want)
		}
	}
}

func assertArgs(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length mismatch got %d want %d\n got: %v\nwant: %v", label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: arg[%d] got %q want %q", label, i, got[i], want[i])
		}
	}
}
