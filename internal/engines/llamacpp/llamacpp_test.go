package llamacpp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// winget-like and CUDA-fork-like output captured from the reference box (2026-07-06). The
// fake binaries below reproduce these shapes so the parser is tested against reality.
const (
	wingetVersionOut = `load_backend: loaded RPC backend from C:\path\ggml-rpc.dll
load_backend: loaded Vulkan backend from C:\path\ggml-vulkan.dll
load_backend: loaded CPU backend from C:\path\ggml-cpu-haswell.dll
version: 8833 (45cac7ca7)
built with Clang 19.1.5 for Windows x86_64`

	wingetHelpOut = `-fa,   --flash-attn [on|off|auto]   set Flash Attention use
-fit,  --fit [on|off]               whether to adjust unset arguments to fit in device memory
--sleep-idle-seconds SECONDS        number of seconds of idleness after which the server will sleep`

	forkVersionOut = `ggml_cuda_init: found 1 CUDA devices (Total VRAM: 16379 MiB):
  Device 0: NVIDIA GeForce RTX 4060 Ti, compute capability 8.9, VMM: yes, VRAM: 16379 MiB
version: 1 (1cfa891)
built with MSVC 19.44.35224.0 for Windows AMD64`

	forkHelpOut = `-fa,   --flash-attn [on|off|auto]   set Flash Attention use
-ctv,  --cache-type-v TYPE          KV cache data type for V
                                    tq3_0
-fit,  --fit [on|off]               whether to adjust unset arguments to fit in device memory
--sleep-idle-seconds SECONDS        number of seconds of idleness after which the server will sleep`
)

// modernVersionOut reproduces the verified b10106 `--version` shape (2026-07-25): build number
// + commit + a "built with" line, and NO backend banner at all — modern llama.cpp loads its
// backends (ggml-cuda.dll etc.) as dynamic sibling libraries. This is the defect M3.7/B fixes.
const modernVersionOut = `version: 10106 (1425386fd)
built with Clang 20.1.8 for Windows x86_64`

// modernHelpOut carries the capability flags the b10106 build exposes (verified from its real
// --help). Only the tokens parseCapabilities greps need to be present.
const modernHelpOut = `-fa,   --flash-attn [on|off|auto]   set Flash Attention use
-fit,  --fit [on|off]               whether to adjust unset arguments to fit in device memory
--sleep-idle-seconds SECONDS        number of seconds of idleness after which the server will sleep
-ctk,  --cache-type-k TYPE          KV cache data type for K
-ctv,  --cache-type-v TYPE          KV cache data type for V
--spec-draft-n-max N                number of tokens to draft for speculative decoding
--spec-type none,draft-simple,draft-eagle3,draft-mtp,ngram-cache`

// writeFakeLlama writes a fake llama-server that prints versionOut on --version and helpOut
// on --help (both to stderr, matching real llama.cpp which writes there).
func writeFakeLlama(t *testing.T, name, versionOut, helpOut string) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, name+".bat")
		var b strings.Builder
		b.WriteString("@echo off\r\n")
		b.WriteString("if \"%1\"==\"--version\" (\r\n")
		for _, line := range strings.Split(versionOut, "\n") {
			b.WriteString("  echo " + batEscape(line) + " 1>&2\r\n")
		}
		b.WriteString("  exit /b 0\r\n)\r\n")
		b.WriteString("if \"%1\"==\"--help\" (\r\n")
		for _, line := range strings.Split(helpOut, "\n") {
			b.WriteString("  echo " + batEscape(line) + " 1>&2\r\n")
		}
		b.WriteString("  exit /b 0\r\n)\r\n")
		b.WriteString("exit /b 0\r\n")
		if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := filepath.Join(dir, name)
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("if [ \"$1\" = \"--version\" ]; then cat >&2 <<'EOF'\n" + versionOut + "\nEOF\nexit 0; fi\n")
	b.WriteString("if [ \"$1\" = \"--help\" ]; then cat >&2 <<'EOF'\n" + helpOut + "\nEOF\nexit 0; fi\n")
	b.WriteString("exit 0\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// batEscape neutralizes characters that break `echo` in a .bat (parentheses, pipes, redirs).
func batEscape(s string) string {
	r := strings.NewReplacer("(", "^(", ")", "^)", "|", "^|", "<", "^<", ">", "^>", "&", "^&")
	return r.Replace(s)
}

func TestDetect_WingetLike(t *testing.T) {
	bin := writeFakeLlama(t, "llama-server", wingetVersionOut, wingetHelpOut)
	a := New([]BinarySpec{{Path: bin}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	sts := a.Detect(ctx)

	if len(sts) != 1 {
		t.Fatalf("want 1 status, got %d", len(sts))
	}
	st := sts[0]
	if !st.Detected || !st.Reachable {
		t.Fatalf("want detected, got %+v", st)
	}
	if !strings.Contains(st.Version, "8833") {
		t.Errorf("version: got %q want to contain 8833", st.Version)
	}
	backends, _ := st.Details["backends"].([]string)
	if !containsStr(backends, "Vulkan") || !containsStr(backends, "CPU") {
		t.Errorf("backends should include Vulkan+CPU, got %v", backends)
	}
	if containsStr(backends, "CUDA") {
		t.Errorf("winget build must NOT report CUDA, got %v", backends)
	}
	caps, _ := st.Details["capabilities"].(map[string]bool)
	if !caps["has_flash_attn"] || !caps["has_fit"] || !caps["has_sleep_idle"] {
		t.Errorf("winget caps wrong: %v", caps)
	}
	if caps["has_tq3"] {
		t.Errorf("winget build must NOT report tq3, got %v", caps)
	}
}

func TestDetect_CUDAForkLike(t *testing.T) {
	bin := writeFakeLlama(t, "llama-server", forkVersionOut, forkHelpOut)
	a := New([]BinarySpec{{Path: bin, ExtraPath: []string{filepath.Dir(bin)}}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	sts := a.Detect(ctx)

	st := sts[0]
	if !st.Detected {
		t.Fatalf("want detected, got %+v", st)
	}
	if !strings.Contains(st.Version, "1cfa891") {
		t.Errorf("version: got %q want commit 1cfa891", st.Version)
	}
	backends, _ := st.Details["backends"].([]string)
	if !containsStr(backends, "CUDA") {
		t.Errorf("fork must report CUDA, got %v", backends)
	}
	caps, _ := st.Details["capabilities"].(map[string]bool)
	if !caps["has_tq3"] {
		t.Errorf("fork must report has_tq3=true, got %v", caps)
	}
	if !caps["has_flash_attn"] {
		t.Errorf("fork must report flash_attn, got %v", caps)
	}
	// ExtraPath recorded in details.
	if _, ok := st.Details["extra_path"]; !ok {
		t.Error("extra_path should be recorded in details for fork")
	}
}

func TestDetect_MissingBinary(t *testing.T) {
	a := New([]BinarySpec{{Path: filepath.Join(t.TempDir(), "no-such-llama-server")}})
	sts := a.Detect(context.Background())
	if len(sts) != 1 {
		t.Fatalf("want 1 status, got %d", len(sts))
	}
	st := sts[0]
	if st.Detected || st.Reachable {
		t.Errorf("missing binary should be not-detected, got %+v", st)
	}
	if st.Reason == "" {
		t.Error("Reason must be non-empty on missing binary (non-waivable)")
	}
}

func TestDetect_NoCandidates(t *testing.T) {
	a := New(nil)
	sts := a.Detect(context.Background())
	if len(sts) != 1 || sts[0].Detected || sts[0].Reason == "" {
		t.Errorf("no candidates should yield one not-detected status with reason, got %+v", sts)
	}
}

func TestNewFromPaths_ExtraPathOnlyExplicit(t *testing.T) {
	extra := []string{`K:\Softwares\Nvidia GPU Compiling Toolkit\CUDA\v12.5\bin`}
	a := NewFromPaths([]string{"llama-server", `K:\fork\llama-server.exe`}, extra)
	if len(a.Specs) != 2 {
		t.Fatalf("want 2 specs, got %d", len(a.Specs))
	}
	// bare name gets no extra path.
	if len(a.Specs[0].ExtraPath) != 0 {
		t.Errorf("bare PATH name should get no ExtraPath, got %v", a.Specs[0].ExtraPath)
	}
	// explicit path gets the extra path.
	if len(a.Specs[1].ExtraPath) != 1 {
		t.Errorf("explicit path should get ExtraPath, got %v", a.Specs[1].ExtraPath)
	}
}

func TestParseVersion(t *testing.T) {
	v, sha := parseVersion(wingetVersionOut)
	if v != "8833 (45cac7ca7)" || sha != "45cac7ca7" {
		t.Errorf("parseVersion winget: v=%q sha=%q", v, sha)
	}
	v, sha = parseVersion(forkVersionOut)
	if v != "1 (1cfa891)" || sha != "1cfa891" {
		t.Errorf("parseVersion fork: v=%q sha=%q", v, sha)
	}
	if v, _ := parseVersion("no version here"); v != "" {
		t.Errorf("parseVersion on garbage should be empty, got %q", v)
	}
}

// --- M3.7/B: co-located DLL inference for builds that advertise no backends -----------------

// writeDLLs writes empty files named after the given ggml backend libraries into dir.
func writeDLLs(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackendsFromDir_InfersCUDAFromColocatedDLL(t *testing.T) {
	dir := t.TempDir()
	writeDLLs(t, dir, "ggml-base.dll", "ggml-cuda.dll", "ggml-cpu-haswell.dll", "ggml-rpc.dll")
	got := backendsFromDir(filepath.Join(dir, "llama-server.exe"))
	if !containsStr(got, "CUDA") {
		t.Errorf("co-located ggml-cuda.dll must infer CUDA, got %v", got)
	}
	if !containsStr(got, "CPU") {
		t.Errorf("co-located ggml-cpu-*.dll must infer CPU, got %v", got)
	}
	if containsStr(got, "base") { // ggml-base.dll is NOT a backend
		t.Errorf("ggml-base.dll must not be reported as a backend, got %v", got)
	}
}

func TestBackendsFromDir_NoGGMLFiles(t *testing.T) {
	dir := t.TempDir()
	writeDLLs(t, dir, "unrelated.dll", "llama-server.exe")
	if got := backendsFromDir(filepath.Join(dir, "llama-server.exe")); len(got) != 0 {
		t.Errorf("dir with no ggml-* libs must yield no backends, got %v", got)
	}
}

func TestBackendsFromDir_NormalizesVulkanRPC(t *testing.T) {
	dir := t.TempDir()
	writeDLLs(t, dir, "ggml-vulkan.dll", "ggml-rpc.dll")
	got := backendsFromDir(filepath.Join(dir, "llama-server.exe"))
	if !containsStr(got, "Vulkan") || !containsStr(got, "RPC") {
		t.Errorf("expected normalized Vulkan+RPC, got %v", got)
	}
}

// TestDetect_ModernBuildNoBackendBanner_InfersCUDA is the core M3.7/B regression: a modern
// llama.cpp build whose `--version` prints NO backend line is still detected as CUDA via its
// co-located ggml-cuda.dll, so it stops being misread as backend-less / has_cuda=false. Without
// the fix, parseBackends returns [] and HasCUDA is false — which used to make b10106 lose
// selection to the fork. The co-located DLL is the verified primary-source signal.
func TestDetect_ModernBuildNoBackendBanner_InfersCUDA(t *testing.T) {
	bin := writeFakeLlama(t, "llama-server", modernVersionOut, modernHelpOut)
	// Place a co-located ggml-cuda.dll beside the fake binary, exactly as b10106 ships.
	writeDLLs(t, filepath.Dir(bin), "ggml-cuda.dll", "ggml-base.dll", "ggml-cpu-haswell.dll")
	a := New([]BinarySpec{{Path: bin}})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	sts := a.Detect(ctx)

	st := sts[0]
	if !st.Detected {
		t.Fatalf("want detected, got %+v", st)
	}
	if !strings.Contains(st.Version, "10106") || !strings.Contains(st.Version, "1425386fd") {
		t.Errorf("version: got %q want 10106 (1425386fd)", st.Version)
	}
	backends, _ := st.Details["backends"].([]string)
	if !containsStr(backends, "CUDA") {
		t.Errorf("modern build with co-located ggml-cuda.dll MUST report CUDA, got %v", backends)
	}
	// The reason must NOT say "backends: none reported" (the pre-fix symptom).
	if strings.Contains(st.Reason, "none reported") {
		t.Errorf("reason must not report 'none reported' for a CUDA build, got %q", st.Reason)
	}
	caps, _ := st.Details["capabilities"].(map[string]bool)
	if !caps["has_flash_attn"] || !caps["has_fit"] {
		t.Errorf("modern build caps wrong: %v", caps)
	}
	if caps["has_tq3"] {
		t.Errorf("modern build must NOT report tq3, got %v", caps)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
