// This file defines the DockerClient seam the vLLM runner uses so tests can drive the whole
// start/stop/reconcile contract WITHOUT a real docker daemon (P4.4: contract-level, no real
// vLLM). The real implementation (execDocker) shells out to the docker CLI; a fake in tests
// records the commands and returns canned output. The runner NEVER calls exec directly — every
// docker interaction goes through this interface, so the grep audit ("no docker run/pull in
// tests") holds: tests inject the fake, only execDocker calls exec.Command, and the real E2E
// (P4.5) is the ONLY place execDocker runs `docker run` for real.
package vllm

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/localhub/localhub/internal/winproc"
)

// DockerClient runs docker subcommands. Run executes an argv (without the leading "docker") and
// returns combined stdout+stderr. It is the single seam between the runner and the docker CLI.
type DockerClient interface {
	// Run executes `docker <args...>` and returns its combined output. A non-zero exit is an error
	// (with the output folded into the message for a readable reason).
	Run(ctx context.Context, args ...string) (string, error)
}

// execDocker is the real DockerClient: it shells out to the docker CLI. This is the ONLY type that
// executes docker; it runs `docker run` only when the runner is actually asked to start a
// container (P4.5), never in unit tests.
type execDocker struct {
	// Path is the docker binary (default "docker", resolved via PATH).
	Path string
}

// NewExecDocker builds the real docker client. An empty path defaults to "docker".
func NewExecDocker(path string) DockerClient {
	if strings.TrimSpace(path) == "" {
		path = "docker"
	}
	return &execDocker{Path: path}
}

func (d *execDocker) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.Path, args...)
	// No console window: this is the single choke point for every docker CLI call, and the
	// engines probe runs `docker images` on each poll. The daemon has no console of its own
	// (-H=windowsgui), so an unhidden child gets a brand new visible window.
	winproc.Hide(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		snippet := strings.TrimSpace(out)
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		return out, fmt.Errorf("docker %s: %w (%s)", strings.Join(args, " "), err, snippet)
	}
	return out, nil
}
