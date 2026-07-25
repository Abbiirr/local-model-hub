// Package vllm is the read-only vLLM detection adapter (P1.3). vLLM has no native Windows
// support (PLAN.md resolution 4): on the reference box only "attach" (a pre-existing vLLM
// endpoint) and "docker" (a container LocalHub would run — later) are usable. THIS package
// only detects; it spawns no container and pulls no image. Its external commands are
// read-only: HTTP GETs for attach, and `docker --version` + `docker images` for docker
// mode. It NEVER runs `docker run` or `docker pull` — that mutation is Phase 4 (P4.5).
//
// Detection by mode:
//   - attach: GET <endpoint>/health, then GET <endpoint>/version. Answering -> Detected=
//     true, Reachable=true. Not answering -> Detected=false (nothing configured to attach
//     to that is up), with a Reason.
//   - docker: check `docker --version` works (docker CLI present) and whether a vLLM image
//     exists locally via `docker images --format ...` filtered for "vllm". No image ->
//     Detected=false with the P4.5 provisioning reason (per task spec). Docker CLI absent
//     -> Detected=false, Reachable=false with a Reason.
//
// vLLM serve args / sleep-mode gate are Phase 4 concerns and are NOT re-verified here (this
// task is detection only); those get re-verified against current vLLM docs at P4.4/P4.5.
package vllm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"github.com/localhub/localhub/internal/engines"
)

// Mode selects how the adapter reaches vLLM.
type Mode string

const (
	ModeDocker     Mode = "docker"
	ModeAttach     Mode = "attach"
	ModeSubprocess Mode = "subprocess"
)

// DefaultImageMatch is the substring used to recognize a locally-present vLLM docker image
// (repository name). Any local image whose repository contains this token counts as a vLLM
// image for detection purposes.
const DefaultImageMatch = "vllm"

// Adapter detects vLLM in either attach or docker mode. Construct with New. Read-only.
type Adapter struct {
	Mode     Mode
	Endpoint string // attach mode: the pre-existing vLLM base URL
	// ImageMatch is the substring used to match a local docker image repository (docker
	// mode). Empty falls back to DefaultImageMatch.
	ImageMatch string
	HTTP       *http.Client
	// DockerPath is the docker CLI to invoke (default "docker", resolved via PATH).
	DockerPath string
}

// New builds an Adapter. mode is normalized to lower-case; endpoint is the attach URL
// (attach mode only).
func New(mode, endpoint string) *Adapter {
	return &Adapter{
		Mode:       Mode(strings.ToLower(strings.TrimSpace(mode))),
		Endpoint:   strings.TrimRight(strings.TrimSpace(endpoint), "/"),
		ImageMatch: DefaultImageMatch,
		HTTP:       &http.Client{},
		DockerPath: "docker",
	}
}

// Kind implements the engine kind for this adapter.
func (a *Adapter) Kind() engines.EngineKind { return engines.KindVLLM }

// Detect performs read-only detection for the configured mode and returns one status.
func (a *Adapter) Detect(ctx context.Context) engines.EngineStatus {
	switch a.Mode {
	case ModeAttach:
		return a.detectAttach(ctx)
	case ModeDocker:
		return a.detectDocker(ctx)
	default:
		return engines.EngineStatus{
			Kind:      engines.KindVLLM,
			Detected:  false,
			Reachable: false,
			Reason: fmt.Sprintf(
				"vllm mode %q not supported for detection (want attach|docker); vLLM has no native Windows support (PLAN.md).",
				string(a.Mode)),
		}
	}
}

// detectAttach probes a pre-existing vLLM endpoint. GET /health then GET /version.
func (a *Adapter) detectAttach(ctx context.Context) engines.EngineStatus {
	st := engines.EngineStatus{
		Kind:     engines.KindVLLM,
		Endpoint: a.Endpoint,
		Details:  map[string]any{"mode": "attach"},
	}
	if a.Endpoint == "" {
		st.Detected = false
		st.Reachable = false
		st.Reason = "vllm mode=attach but no endpoint configured; nothing to attach to."
		return st
	}

	if err := a.getOK(ctx, a.Endpoint+"/health"); err != nil {
		st.Detected = false
		st.Reachable = false
		st.Reason = fmt.Sprintf("vllm attach endpoint %s/health did not answer (%v); no vLLM server reachable there.", a.Endpoint, err)
		return st
	}

	// Health passed -> a vLLM server is answering.
	st.Detected = true
	st.Reachable = true

	// /version is best-effort; a healthy vLLM answers it, but a failure must not downgrade
	// a passing /health.
	if ver, err := a.version(ctx); err == nil && ver != "" {
		st.Version = ver
	} else if err != nil {
		st.Details["version_error"] = err.Error()
	}
	st.Reason = fmt.Sprintf("vLLM reachable at %s (mode=attach, /health OK).", a.Endpoint)
	return st
}

// detectDocker checks docker CLI presence and local vLLM image presence. No run, no pull.
func (a *Adapter) detectDocker(ctx context.Context) engines.EngineStatus {
	st := engines.EngineStatus{
		Kind:    engines.KindVLLM,
		Details: map[string]any{"mode": "docker"},
	}
	dockerPath := a.DockerPath
	if strings.TrimSpace(dockerPath) == "" {
		dockerPath = "docker"
	}

	// 1) docker CLI present?
	verOut, verErr := runCmd(ctx, dockerPath, "--version")
	if verErr != nil {
		st.Detected = false
		st.Reachable = false
		st.Reason = fmt.Sprintf("vllm mode=docker but docker CLI (%s) is not usable (%v); cannot detect or provision vLLM.", dockerPath, verErr)
		return st
	}
	st.Details["docker_version"] = strings.TrimSpace(verOut)

	// 2) local vLLM image present? `docker images --format {{.Repository}}:{{.Tag}}`.
	imgOut, imgErr := runCmd(ctx, dockerPath, "images", "--format", "{{.Repository}}:{{.Tag}}")
	if imgErr != nil {
		// Docker CLI exists but listing failed (daemon down?). Report not-detected with
		// the specific error rather than pretending.
		st.Detected = false
		st.Reachable = false
		st.Reason = fmt.Sprintf("vllm mode=docker: docker CLI present but `docker images` failed (%v); is the Docker daemon running?", imgErr)
		return st
	}

	match := a.ImageMatch
	if match == "" {
		match = DefaultImageMatch
	}
	images := matchImages(imgOut, match)
	if len(images) > 0 {
		st.Detected = true
		st.Reachable = false // image present but no container spawned (detection is read-only)
		st.Details["images"] = images
		st.Reason = fmt.Sprintf(
			"vLLM docker image(s) present locally (%s); not started (detection is read-only). Will be launched in Phase 4 (P4.5).",
			strings.Join(images, ", "))
		return st
	}

	// This is the current reference-box state (task spec): docker mode configured, no image.
	st.Detected = false
	st.Reachable = false
	st.Details["images"] = []string{}
	st.Reason = "vllm docker mode configured; no vllm image present; will be provisioned in P4.5."
	return st
}

// Version performs GET <endpoint>/version and returns the version string. Read-only.
func (a *Adapter) Version(ctx context.Context) (string, error) { return a.version(ctx) }

// versionResponse mirrors vLLM's GET /version, which returns {"version":"x.y.z"}. Parsed
// defensively (unknown fields ignored, missing fields zero).
type versionResponse struct {
	Version string `json:"version"`
}

func (a *Adapter) version(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Endpoint+"/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET /version returned HTTP %d", resp.StatusCode)
	}
	var v versionResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// getOK issues a GET and returns nil only on a 2xx response (used for /health).
func (a *Adapter) getOK(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// matchImages returns image references from `docker images --format` output whose text
// contains match (case-insensitive). Empty/`<none>:<none>` lines are skipped.
func matchImages(out, match string) []string {
	match = strings.ToLower(match)
	var res []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || l == "<none>:<none>" {
			continue
		}
		if strings.Contains(strings.ToLower(l), match) {
			res = append(res, l)
		}
	}
	return res
}

// runCmd runs a read-only command with the context timeout and returns combined output.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
