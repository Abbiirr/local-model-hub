// Package llamacpp is the read-only llama.cpp (llama-server) detection adapter (P1.3).
// LocalHub owns llama-server processes it spawns (engine-adapters.md §5), but THIS package
// only detects and capability-probes binaries — it spawns no model. The only commands it
// runs are `<binary> --version` and `<binary> --help`, both of which load no model and
// change no state (verified on the reference box, 2026-07-06). Model spawning is Phase 4.
//
// Detection strategy (per binary candidate):
//   - Resolve the candidate to an absolute path (PATH lookup for a bare name like
//     "llama-server"; explicit paths used as-is).
//   - Run `--version` (short timeout). llama.cpp prints the version to stderr in the form
//     "version: <build> (<sha>)" and prints loaded backends before it. We capture BOTH
//     streams because the version and backend hints land on stderr.
//   - Run `--help` and grep for capability flags.
//   - Backends: parse the `--version` banner (load_backend:/ggml_cuda_init lines). A MODERN
//     build loads its backends as dynamic ggml-*.dll siblings and prints NO banner, so when
//     `--version` advertised nothing we fall back to inferring backends from the co-located
//     ggml-*.dll/.so/.dylib libraries (backendsFromDir) — the fix for a CUDA build being
//     misread as backend-less / has_cuda=false (build 10106, verified 2026-07-25).
//
// CAPABILITY PROBING (lmwrapper reuse pattern, research/lmwrapper-reuse-notes.md §4): the
// two binaries on this box differ. We must NOT assume a flag exists — we grep --help/
// --version for it. Verified real output shapes (2026-07-06):
//
//	winget build (ggml b8833, Vulkan/CPU — NO CUDA):
//	  stderr: "load_backend: loaded RPC backend from ...ggml-rpc.dll"
//	          "load_backend: loaded Vulkan backend from ...ggml-vulkan.dll"
//	          "load_backend: loaded CPU backend from ...ggml-cpu-haswell.dll"
//	          "version: 8833 (45cac7ca7)"
//	  --help has: --flash-attn, --fit, --sleep-idle-seconds ; NO "tq3"
//
//	CUDA fork (llama.cpp-tq3, build 1cfa891 — CUDA, TQ3 kernels):
//	  stderr: "ggml_cuda_init: found 1 CUDA devices (Total VRAM: 16379 MiB):"
//	          "  Device 0: NVIDIA GeForce RTX 4060 Ti, compute capability 8.9, ..."
//	          "version: 1 (1cfa891)"
//	  --help has: --flash-attn, --fit, --sleep-idle-seconds, and "tq3_0" (as a -ctv/-ctk
//	  cache type) ; the CUDA fork needs the CUDA v12.5 bin dir prepended to PATH so its
//	  DLLs load (ExtraPath, below).
//
// PLAN.md correction (lmwrapper notes §fork facts): `--fit` and `--sleep-idle-seconds` are
// now UPSTREAM (winget has them). Genuinely fork-only is the tq3_0 KV cache type + TQ3
// quant kernels + this build's CUDA backend. So has_tq3 and the CUDA backend hint are the
// signals that route TQ3 models / -ctv tq3_0 to the fork.
package llamacpp

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/localhub/localhub/internal/engines"
	"github.com/localhub/localhub/internal/winproc"
)

// BinarySpec is one candidate llama-server binary to probe. Path is a bare name (resolved
// via PATH) or an absolute path. ExtraPath is an OPTIONAL list of directories prepended to
// PATH when running this binary — required for the CUDA fork, whose CUDA DLLs live in
// "K:\Softwares\Nvidia GPU Compiling Toolkit\CUDA\v12.5\bin" and must be discoverable at
// probe time (recorded in temp-research/wiring-p13.md as a config need).
type BinarySpec struct {
	Path      string
	ExtraPath []string
}

// Capabilities are boolean feature flags detected by grepping --help/--version output.
//
// [RE-VERIFIED 2026-07-25 against the three LOCAL binaries' --help] The MTP/spec and KV bits
// below discriminate correctly on this box:
//   - b10106 (tools/llama.cpp): `--spec-type none,draft-simple,draft-eagle3,draft-mtp,
//     draft-dflash,ngram-simple,ngram-map-k,ngram-map-k4v,ngram-mod,ngram-cache`,
//     `--spec-draft-n-max N` (default 3), `-ctk/-ctv`, `-np/--parallel N` → has_spec_mtp=true.
//   - the llama.cpp-tq3 fork and the winget build: `--spec-type` and `-ctk/-ctv` present but
//     NO `draft-mtp` token and NO `--spec-draft-n-max` → has_spec_mtp=false.
//
// These are PINNED-BUILD observations, not timeless llama.cpp facts (PLAN §4 D8): re-run the
// --help diff after any engine upgrade (docs/22-engine-version-policy.md).
type Capabilities struct {
	HasFit       bool `json:"has_fit"`        // --fit (fit-to-VRAM auto-tuning)
	HasSleepIdle bool `json:"has_sleep_idle"` // --sleep-idle-seconds
	HasTQ3       bool `json:"has_tq3"`        // tq3_0 KV cache / TQ3 quant kernels (fork-only)
	HasFlashAttn bool `json:"has_flash_attn"` // --flash-attn
	// HasSpecMTP: `--spec-type` advertises `draft-mtp` — the EMBEDDED multi-token-prediction
	// head (no separate draft model). Gates the MTP launch profile's binary side (M3.1/M3.3).
	HasSpecMTP bool `json:"has_spec_mtp"`
	// HasSpecDraftMax: `--spec-draft-n-max N` exists (the draft-token count for a spec mode).
	// Independent of HasSpecMTP: a build may expose one without the other, so we never infer
	// one from the other.
	HasSpecDraftMax bool `json:"has_spec_draft_max"`
	// HasKVCacheTypes: BOTH `--cache-type-k` (-ctk) and `--cache-type-v` (-ctv) exist, i.e. the
	// binary can quantize K and V INDEPENDENTLY. The Qwen3.6 profile PROVEN at 64K on this box is
	// symmetric (-ctk q4_0 -ctv q4_0, scripts/run-qwen-64k-q4q4.ps1); an asymmetric pair such as
	// -ctk q8_0 -ctv q4_0 is what independent support additionally buys. A binary without BOTH
	// flags must be launched with an f16 cache.
	HasKVCacheTypes bool `json:"has_kv_cache_types"`
	// HasParallelSlots: `-np`/`--parallel` exists (server slot count; draft-mtp pins it to 1).
	HasParallelSlots bool `json:"has_parallel_slots"`
}

// Adapter probes a set of candidate llama-server binaries. Construct with New. Read-only.
type Adapter struct {
	Specs []BinarySpec
}

// New builds an Adapter from candidate binaries. Bare names and absolute paths are both
// accepted; ExtraPath is optional per spec.
func New(specs []BinarySpec) *Adapter { return &Adapter{Specs: specs} }

// NewFromPaths is a convenience constructor for the common case where callers only have a
// list of paths and (optionally) a single ExtraPath that applies to explicit-path
// candidates. It mirrors the config shape (Engines.LlamaCPP.BinaryPaths + an ExtraPath for
// the CUDA fork). Bare names get no ExtraPath; absolute paths get the supplied extraPath.
func NewFromPaths(paths []string, extraPath []string) *Adapter {
	specs := make([]BinarySpec, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		spec := BinarySpec{Path: p}
		// Only attach ExtraPath to absolute/explicit paths; a bare PATH name is the
		// system build that needs no extra DLL directory.
		if extraPath != nil && isExplicitPath(p) {
			spec.ExtraPath = extraPath
		}
		specs = append(specs, spec)
	}
	return &Adapter{Specs: specs}
}

// Kind implements the engine kind for this adapter.
func (a *Adapter) Kind() engines.EngineKind { return engines.KindLlamaCPP }

// versionRe matches the llama.cpp version line, e.g. "version: 8833 (45cac7ca7)" or
// "version: 1 (1cfa891)". Group 1 is the build number/id, group 2 the short commit sha.
var versionRe = regexp.MustCompile(`(?m)^version:\s*(\S+)\s*\(([^)]*)\)`)

// Detect probes every candidate binary and returns one EngineStatus per binary. A binary
// that cannot be resolved or does not answer --version yields Detected=false with a Reason;
// a binary that answers yields Detected=true (Reachable is used here to mean "the binary
// executed and reported a version" — llama-server is a spawned engine, so there is no
// long-running daemon to be reachable, but a runnable binary is the analog).
//
// Concurrency is 1 (probes run sequentially) and each probe has a tight timeout supplied by
// the caller's context, per the hardware-safeguard rule (running --version/--help loads no
// model but we still keep it cheap and serial).
func (a *Adapter) Detect(ctx context.Context) []engines.EngineStatus {
	out := make([]engines.EngineStatus, 0, len(a.Specs))
	for _, spec := range a.Specs {
		out = append(out, a.detectOne(ctx, spec))
	}
	if len(out) == 0 {
		out = append(out, engines.EngineStatus{
			Kind:      engines.KindLlamaCPP,
			Detected:  false,
			Reachable: false,
			Reason:    "no llama-server binary candidates configured; cannot detect llama.cpp.",
		})
	}
	return out
}

func (a *Adapter) detectOne(ctx context.Context, spec BinarySpec) engines.EngineStatus {
	st := engines.EngineStatus{
		Kind:    engines.KindLlamaCPP,
		Details: map[string]any{"binary_path": spec.Path},
	}
	if len(spec.ExtraPath) > 0 {
		st.Details["extra_path"] = spec.ExtraPath
	}

	// Resolve to an absolute path (PATH lookup for bare names). This also detects
	// "binary not present" before we try to run it.
	resolved, rerr := resolveBinary(spec.Path)
	if rerr != nil {
		st.Detected = false
		st.Reachable = false
		st.Reason = "llama-server binary " + quote(spec.Path) + " not found: " + rerr.Error()
		return st
	}
	st.Details["resolved_path"] = resolved

	// Run --version (captures version + backend hints, mostly on stderr).
	verOut, verErr := runProbe(ctx, resolved, spec.ExtraPath, "--version")
	if verErr != nil {
		st.Detected = false
		st.Reachable = false
		st.Reason = "llama-server " + quote(resolved) + " failed to run --version: " + verErr.Error()
		return st
	}

	// Run --help for capability probing. If --help fails we still have a runnable binary;
	// combine both outputs for capability grep and note the help error.
	helpOut, helpErr := runProbe(ctx, resolved, spec.ExtraPath, "--help")
	combined := verOut
	if helpErr == nil {
		combined = verOut + "\n" + helpOut
	} else {
		st.Details["help_error"] = helpErr.Error()
	}

	version, sha := parseVersion(verOut)
	backends := parseBackends(verOut)
	// Modern llama.cpp loads compute backends as DYNAMIC sibling libraries
	// (ggml-cuda.dll, ggml-vulkan.dll, ...) and its `--version` prints NO backend
	// banner at all (verified on the reference box, build 10106/1425386fd, 2026-07-25:
	// --version prints only "version: 10106 (1425386fd)" / "built with Clang 20.1.8").
	// The older fork (static CUDA) and the winget build (static Vulkan/CPU) DO print
	// banners, so parseBackends already returns the right set for them. We only
	// AUGMENT from the co-located backend DLLs when --version advertised nothing —
	// that is the exact defect (a CUDA build misread as backend-less / has_cuda=false,
	// which used to make it lose binary selection). This is a filesystem probe of the
	// binary's own directory, not an engine-behavior claim; presence of ggml-cuda.dll
	// means the build supports the CUDA backend. HasCUDA is now an accurate REPORTING
	// field and (after M3.6) no longer a selection criterion.
	if len(backends) == 0 {
		backends = backendsFromDir(resolved)
	}
	caps := parseCapabilities(combined)

	st.Detected = true
	st.Reachable = true // binary executed and reported a version
	st.Version = version
	if sha != "" {
		st.Details["commit"] = sha
	}
	st.Details["backends"] = backends
	st.Details["capabilities"] = map[string]bool{
		"has_fit":            caps.HasFit,
		"has_sleep_idle":     caps.HasSleepIdle,
		"has_tq3":            caps.HasTQ3,
		"has_flash_attn":     caps.HasFlashAttn,
		"has_spec_mtp":       caps.HasSpecMTP,
		"has_spec_draft_max": caps.HasSpecDraftMax,
		"has_kv_cache_types": caps.HasKVCacheTypes,
		"has_parallel_slots": caps.HasParallelSlots,
	}

	st.Reason = buildReason(version, backends, caps)
	return st
}

// Version runs `<binary> --version` and returns the parsed version string. Read-only.
func Version(ctx context.Context, spec BinarySpec) (string, error) {
	resolved, err := resolveBinary(spec.Path)
	if err != nil {
		return "", err
	}
	out, err := runProbe(ctx, resolved, spec.ExtraPath, "--version")
	if err != nil {
		return "", err
	}
	v, _ := parseVersion(out)
	return v, nil
}

// parseVersion extracts (build, sha) from "version: <build> (<sha>)". Returns empty
// strings if the line is absent. The full raw form "<build> (<sha>)" is returned as the
// version so callers keep the reproducibility-relevant identifiers.
func parseVersion(out string) (version, sha string) {
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return "", ""
	}
	build, sha := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
	if sha != "" {
		return build + " (" + sha + ")", sha
	}
	return build, ""
}

// parseBackends extracts loaded/available compute backends from --version output. It
// recognizes two shapes seen on the reference box:
//   - winget: "load_backend: loaded Vulkan backend from ...", one line per backend.
//   - CUDA fork: "ggml_cuda_init: found N CUDA devices ..." + "Device 0: NVIDIA ...".
//
// Returns a de-duplicated, order-preserving list like ["Vulkan","CPU","RPC"] or ["CUDA"].
func parseBackends(out string) []string {
	seen := map[string]bool{}
	var backends []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		backends = append(backends, name)
	}

	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		low := strings.ToLower(l)
		switch {
		case strings.HasPrefix(low, "load_backend:"):
			// "load_backend: loaded Vulkan backend from ..." -> word after "loaded".
			if name := backendFromLoadLine(l); name != "" {
				add(name)
			}
		case strings.Contains(low, "ggml_cuda_init") || strings.Contains(low, "cuda device"):
			add("CUDA")
		}
	}
	return backends
}

// loadBackendRe pulls the backend name out of "load_backend: loaded <Name> backend from ...".
var loadBackendRe = regexp.MustCompile(`(?i)loaded\s+(\S+)\s+backend`)

func backendFromLoadLine(line string) string {
	m := loadBackendRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	name := m[1]
	// Normalize a couple of well-known names for a clean UI.
	switch strings.ToLower(name) {
	case "cpu":
		return "CPU"
	case "vulkan":
		return "Vulkan"
	case "cuda":
		return "CUDA"
	case "rpc":
		return "RPC"
	case "metal":
		return "Metal"
	default:
		return name
	}
}

// backendsFromDir infers available compute backends from the ggml-<name> shared libraries
// co-located with the binary (ggml-cuda.dll, ggml-vulkan.dll, ...). It is the fallback for
// modern llama.cpp builds whose `--version` advertises no backends because they load every
// backend dynamically. Returns a de-duplicated, order-preserving list; empty when the
// directory has no ggml-* backend libraries (e.g. a statically-linked build, or a temp dir
// in tests). This reads only the binary's own directory — no engine process is spawned.
func backendsFromDir(binaryPath string) []string {
	dir := filepath.Dir(binaryPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var backends []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			backends = append(backends, name)
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if !strings.HasPrefix(name, "ggml-") {
			continue
		}
		// Only shared libraries: .dll (Windows), .so (Linux), .dylib (macOS).
		if !(strings.HasSuffix(name, ".dll") || strings.HasSuffix(name, ".so") || strings.HasSuffix(name, ".dylib")) {
			continue
		}
		// "ggml-cuda.dll" -> "cuda"; "ggml-cpu-haswell.dll" -> "cpu-haswell" -> CPU.
		tok := strings.TrimPrefix(name, "ggml-")
		tok = strings.TrimSuffix(tok, ".dll")
		tok = strings.TrimSuffix(tok, ".so")
		tok = strings.TrimSuffix(tok, ".dylib")
		add(normalizeBackendToken(tok))
	}
	return backends
}

// normalizeBackendToken maps a ggml-<tok> library token to a clean backend label, matching
// the names parseBackends/backendFromLoadLine already produce so the two sources merge
// without duplicates. Returns "" for tokens that are NOT compute backends (notably "base" —
// ggml-base.dll is the shared runtime library, not a backend) so the caller can skip them.
func normalizeBackendToken(tok string) string {
	switch {
	case strings.HasPrefix(tok, "cuda"):
		return "CUDA"
	case strings.HasPrefix(tok, "vulkan"):
		return "Vulkan"
	case strings.HasPrefix(tok, "rpc"):
		return "RPC"
	case strings.HasPrefix(tok, "metal"):
		return "Metal"
	case strings.HasPrefix(tok, "opencl"):
		return "OpenCL"
	case strings.HasPrefix(tok, "sycl"):
		return "SYCL"
	case strings.HasPrefix(tok, "kompute"):
		return "Kompute"
	case strings.HasPrefix(tok, "cann"):
		return "CANN"
	case strings.HasPrefix(tok, "cpu"):
		return "CPU"
	default:
		return "" // not a compute backend (e.g. "base"); skip
	}
}

// parseCapabilities greps the combined --version/--help text for capability signals. All
// checks are case-insensitive substring matches on the flag/type tokens.
//
// The spec/KV bits are matched on the EXACT tokens observed in the b10106 --help (see the
// Capabilities doc comment). Each bit is probed independently — we never infer `draft-mtp`
// support from the presence of `--spec-type`, because the fork and the winget build both
// expose `--spec-type` WITHOUT draft-mtp (verified 2026-07-25). A false has_spec_mtp would
// let a start emit `--spec-type draft-mtp` at a binary that rejects it.
func parseCapabilities(text string) Capabilities {
	low := strings.ToLower(text)
	return Capabilities{
		HasFit:       strings.Contains(low, "--fit"),
		HasSleepIdle: strings.Contains(low, "--sleep-idle-seconds"),
		// tq3 is fork-only: match "tq3" or "turboquant" anywhere in help/version.
		HasTQ3:       strings.Contains(low, "tq3") || strings.Contains(low, "turboquant"),
		HasFlashAttn: strings.Contains(low, "--flash-attn") || strings.Contains(low, "flash-attn"),
		// draft-mtp appears as a value in the `--spec-type` enumeration, not as its own flag.
		HasSpecMTP:      strings.Contains(low, "draft-mtp"),
		HasSpecDraftMax: strings.Contains(low, "--spec-draft-n-max"),
		// Independent K/V needs BOTH sides; a build with only one cannot express -ctk q8_0 -ctv q4_0.
		HasKVCacheTypes: (strings.Contains(low, "--cache-type-k") || strings.Contains(low, "-ctk")) &&
			(strings.Contains(low, "--cache-type-v") || strings.Contains(low, "-ctv")),
		HasParallelSlots: strings.Contains(low, "--parallel") || strings.Contains(low, "-np,"),
	}
}

func buildReason(version string, backends []string, caps Capabilities) string {
	var sb strings.Builder
	sb.WriteString("llama-server detected")
	if version != "" {
		sb.WriteString(" (version " + version + ")")
	}
	if len(backends) > 0 {
		sb.WriteString("; backends: " + strings.Join(backends, ", "))
	} else {
		sb.WriteString("; backends: none reported")
	}
	var capList []string
	if caps.HasFlashAttn {
		capList = append(capList, "flash-attn")
	}
	if caps.HasFit {
		capList = append(capList, "fit")
	}
	if caps.HasSleepIdle {
		capList = append(capList, "sleep-idle")
	}
	if caps.HasTQ3 {
		capList = append(capList, "tq3 (fork)")
	}
	if caps.HasKVCacheTypes {
		capList = append(capList, "kv-types (-ctk/-ctv)")
	}
	if caps.HasSpecMTP {
		capList = append(capList, "spec-mtp (draft-mtp)")
	}
	if caps.HasSpecDraftMax {
		capList = append(capList, "spec-draft-n-max")
	}
	if len(capList) > 0 {
		sb.WriteString("; caps: " + strings.Join(capList, ", "))
	}
	sb.WriteString(".")
	return sb.String()
}

// runProbe runs `binary arg` with the context timeout, prepending extraPath dirs to PATH
// for this invocation only (needed for the CUDA fork's DLLs). It returns the combined
// stdout+stderr because llama.cpp writes the version and backend hints to stderr. A
// non-zero exit is still returned as output+error so callers can decide; but for
// --version/--help llama-server exits 0. Read-only: no model is loaded.
func runProbe(ctx context.Context, binary string, extraPath []string, arg string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, arg)
	// No console window. This is the noisiest spawn site in the daemon: GET /api/engines runs
	// this probe once per DETECTED llama.cpp build, and the dashboard polls that endpoint, so
	// without this a box with three installs flashes three console windows on every refresh.
	winproc.Hide(cmd)
	cmd.Env = envWithExtraPath(extraPath)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
