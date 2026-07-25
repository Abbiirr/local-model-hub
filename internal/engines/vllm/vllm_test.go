package vllm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDetectAttach_Reachable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("mutating call to /health: %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"0.6.3"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := New("attach", srv.URL)
	st := a.Detect(context.Background())
	if !st.Detected || !st.Reachable {
		t.Fatalf("want detected+reachable, got %+v", st)
	}
	if st.Version != "0.6.3" {
		t.Errorf("version: got %q want 0.6.3", st.Version)
	}
	if st.Reason == "" {
		t.Error("reason must not be empty")
	}
}

func TestDetectAttach_Unreachable(t *testing.T) {
	a := New("attach", "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := a.Detect(ctx)
	if st.Detected || st.Reachable {
		t.Errorf("unreachable endpoint should be not-detected, got %+v", st)
	}
	if st.Reason == "" {
		t.Error("Reason must be non-empty on unreachable path (non-waivable)")
	}
}

func TestDetectAttach_NoEndpoint(t *testing.T) {
	a := New("attach", "")
	st := a.Detect(context.Background())
	if st.Detected || st.Reason == "" {
		t.Errorf("attach with no endpoint should be not-detected with reason, got %+v", st)
	}
}

// --- docker mode: use a fake `docker` script via DockerPath ---

// writeFakeDocker writes a script that emulates `docker --version` and `docker images`.
// imagesOut is what `docker images ...` prints (may be empty to simulate no image).
func writeFakeDocker(t *testing.T, imagesOut string, failImages bool) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "docker.bat")
		var b strings.Builder
		b.WriteString("@echo off\r\n")
		b.WriteString("if \"%1\"==\"--version\" (\r\n")
		b.WriteString("  echo Docker version 29.4.0, build test\r\n")
		b.WriteString("  exit /b 0\r\n")
		b.WriteString(")\r\n")
		b.WriteString("if \"%1\"==\"images\" (\r\n")
		if failImages {
			b.WriteString("  echo cannot connect to daemon 1>&2\r\n")
			b.WriteString("  exit /b 1\r\n")
		} else {
			for _, line := range strings.Split(imagesOut, "\n") {
				if line == "" {
					continue
				}
				b.WriteString("  echo " + line + "\r\n")
			}
			b.WriteString("  exit /b 0\r\n")
		}
		b.WriteString(")\r\n")
		b.WriteString("exit /b 0\r\n")
		if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// POSIX shell fallback (CI on Linux).
	path := filepath.Join(dir, "docker")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("if [ \"$1\" = \"--version\" ]; then echo 'Docker version 29.4.0, build test'; exit 0; fi\n")
	if failImages {
		b.WriteString("if [ \"$1\" = \"images\" ]; then echo 'cannot connect to daemon' 1>&2; exit 1; fi\n")
	} else {
		b.WriteString("if [ \"$1\" = \"images\" ]; then printf '%s\\n' \"" + imagesOut + "\"; exit 0; fi\n")
	}
	b.WriteString("exit 0\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDetectDocker_NoImage(t *testing.T) {
	a := New("docker", "")
	a.DockerPath = writeFakeDocker(t, "", false)
	st := a.Detect(context.Background())

	if st.Detected {
		t.Errorf("no vllm image should be Detected=false, got %+v", st)
	}
	if !strings.Contains(st.Reason, "P4.5") {
		t.Errorf("reason should reference P4.5 provisioning: %q", st.Reason)
	}
}

func TestDetectDocker_ImagePresent(t *testing.T) {
	a := New("docker", "")
	a.DockerPath = writeFakeDocker(t, "vllm/vllm-openai:latest\nubuntu:22.04", false)
	st := a.Detect(context.Background())

	if !st.Detected {
		t.Errorf("vllm image present should be Detected=true, got %+v", st)
	}
	if st.Reachable {
		t.Errorf("image present but no container should be Reachable=false, got %+v", st)
	}
	imgs, _ := st.Details["images"].([]string)
	if len(imgs) != 1 || !strings.Contains(imgs[0], "vllm") {
		t.Errorf("expected one vllm image, got %v", st.Details["images"])
	}
	if st.Reason == "" {
		t.Error("reason must not be empty")
	}
}

func TestDetectDocker_ImagesListFails(t *testing.T) {
	a := New("docker", "")
	a.DockerPath = writeFakeDocker(t, "", true)
	st := a.Detect(context.Background())
	if st.Detected {
		t.Errorf("failed image list should be not-detected, got %+v", st)
	}
	if st.Reason == "" {
		t.Error("Reason must be non-empty (non-waivable)")
	}
}

func TestDetectDocker_NoDockerCLI(t *testing.T) {
	a := New("docker", "")
	a.DockerPath = filepath.Join(t.TempDir(), "definitely-not-docker")
	st := a.Detect(context.Background())
	if st.Detected || st.Reachable {
		t.Errorf("missing docker CLI should be not-detected, got %+v", st)
	}
	if st.Reason == "" {
		t.Error("Reason must be non-empty (non-waivable)")
	}
}

func TestDetect_UnknownMode(t *testing.T) {
	a := New("subprocess", "")
	st := a.Detect(context.Background())
	if st.Detected || st.Reason == "" {
		t.Errorf("subprocess mode unsupported for detection should be not-detected w/ reason, got %+v", st)
	}
}
