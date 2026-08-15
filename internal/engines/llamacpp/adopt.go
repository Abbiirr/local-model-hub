package llamacpp

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/localhub/localhub/internal/winproc"
)

// adopt.go DISCOVERS an already-running llama-server so LocalHub can manage it without having
// spawned it.
//
// WHY this exists: the reference box runs the proven Qwen3.6-27B 64K MTP profile from
// scripts/run-qwen-64k-q4q4.ps1 on port 8080. It is healthy and serving, yet LocalHub's estimator
// REFUSES that same profile (`wont_run`) because gguf-parser over-prices a hybrid SSM model's KV
// cache. Predicting whether a running process fits is both unnecessary and wrong — so for these
// instances LocalHub reads the truth instead:
//
//   - the OWNING PID of the listening port (so stop/monitor/reconcile work as for a spawned process),
//   - the process's REAL command line, which yields the actual context, KV types, speculative mode
//     and slot count — no guessing about which profile is live,
//   - the process's ACTUAL per-process VRAM from `nvidia-smi --query-compute-apps`, a measurement
//     rather than an estimate, and exactly what the calibration store wants.
//
// Everything here is READ-ONLY with respect to the discovered process. Nothing in this file starts,
// signals, or stops anything: adopting a model must never disturb it.
//
// Platform note: PID-by-port and command-line lookup are inherently OS-specific. The implementations
// shell out to PowerShell (Get-NetTCPConnection / Win32_Process), matching what the project's own
// launcher script already relies on, and are injected as function values so tests never touch the OS.

// AdoptedProcess is everything discovery could learn about a running llama-server.
type AdoptedProcess struct {
	PID     int
	Port    int
	BaseURL string
	// CommandLine is the raw command line, kept verbatim for the audit trail: it is ground truth for
	// what is actually running, and the source every parsed field below is derived from.
	CommandLine string
	// Flags is the command line in argv form, recorded as the instance's launch_flags so an adopted
	// instance is as reproducible as a spawned one.
	Flags []string

	// Parsed profile — what the process is ACTUALLY running.
	ModelPath    string
	BinaryPath   string
	Ctx          int
	KVTypeK      string
	KVTypeV      string
	SpecMode     string
	SpecDraftMax int
	Slots        int
	NGL          string

	// VRAMMiB is the process's real per-process VRAM, or 0 when it could not be read. A MEASUREMENT
	// of the running process, never an estimate.
	VRAMMiB int
}

// Seams so tests never shell out.
var (
	pidForPortFn  = pidForPortWindows
	commandLineFn = commandLineWindows
	processVRAMFn = processVRAMMiB
)

// DiscoverByPort finds the llama-server listening on port and reads its profile. It errors when
// nothing is listening, when the owner's command line cannot be read, or when the listening process
// is not a llama-server — adopting the wrong process would attach LocalHub's lifecycle management to
// an unrelated program.
func DiscoverByPort(ctx context.Context, host string, port int) (AdoptedProcess, error) {
	pid, err := pidForPortFn(ctx, port)
	if err != nil {
		return AdoptedProcess{}, fmt.Errorf("find the process listening on port %d: %w", port, err)
	}
	if pid == 0 {
		return AdoptedProcess{}, fmt.Errorf("nothing is listening on port %d", port)
	}
	cmdline, err := commandLineFn(ctx, pid)
	if err != nil {
		return AdoptedProcess{}, fmt.Errorf("read the command line of PID %d: %w", pid, err)
	}
	if !looksLikeLlamaServer(cmdline) {
		return AdoptedProcess{}, fmt.Errorf("PID %d owns port %d but does not look like a llama-server "+
			"(its command line names neither llama-server nor a -m/--model GGUF); refusing to adopt an "+
			"unrelated process. Command line: %s", pid, port, truncate(cmdline, 200))
	}

	ap := parseLlamaCommandLine(cmdline)
	ap.PID = pid
	ap.Port = port
	if host == "" {
		host = "127.0.0.1"
	}
	ap.BaseURL = fmt.Sprintf("http://%s:%d", host, port)
	if mib, verr := processVRAMFn(ctx, pid); verr == nil {
		ap.VRAMMiB = mib
	}
	return ap, nil
}

// looksLikeLlamaServer guards against adopting an unrelated process that happens to own the port. It
// requires either the binary name or a model flag — the same identity discipline the project's
// launcher script applies before it will stop anything.
func looksLikeLlamaServer(cmdline string) bool {
	low := strings.ToLower(cmdline)
	if strings.Contains(low, "llama-server") {
		return true
	}
	return (strings.Contains(low, " -m ") || strings.Contains(low, "--model ")) && strings.Contains(low, ".gguf")
}

// parseLlamaCommandLine extracts the live profile from a llama-server command line. It is a pure
// function over the string, so it is fully unit-testable, and deliberately tolerant: an unrecognised
// flag is ignored rather than failing the adoption, because the goal is to DESCRIBE a process that is
// already running, not to validate it.
//
// Both spellings of every flag are accepted (`-c`/`--ctx-size`, `-ctk`/`--cache-type-k`, …) since a
// hand-written launcher may use either.
func parseLlamaCommandLine(cmdline string) AdoptedProcess {
	args := splitArgs(cmdline)
	ap := AdoptedProcess{CommandLine: cmdline, Flags: args}
	if len(args) > 0 {
		ap.BinaryPath = args[0]
	}

	valueAfter := func(names ...string) string {
		for i := 0; i < len(args)-1; i++ {
			for _, n := range names {
				if strings.EqualFold(args[i], n) {
					return args[i+1]
				}
			}
		}
		return ""
	}

	ap.ModelPath = valueAfter("-m", "--model")
	ap.Ctx = atoiOr(valueAfter("-c", "--ctx-size"), 0)
	ap.KVTypeK = strings.ToLower(valueAfter("-ctk", "--cache-type-k"))
	ap.KVTypeV = strings.ToLower(valueAfter("-ctv", "--cache-type-v"))
	ap.SpecMode = strings.ToLower(valueAfter("--spec-type"))
	ap.SpecDraftMax = atoiOr(valueAfter("--spec-draft-n-max"), 0)
	ap.Slots = atoiOr(valueAfter("-np", "--parallel"), 0)
	ap.NGL = valueAfter("-ngl", "--n-gpu-layers", "--gpu-layers")

	// `--spec-type none` means off; normalise to the empty string LocalHub uses internally so an
	// adopted profile compares equal to a natively-started one.
	if ap.SpecMode == "none" {
		ap.SpecMode = ""
	}
	return ap
}

// splitArgs splits a Windows-style command line into argv, honouring double quotes so a model path
// containing spaces survives intact.
func splitArgs(cmdline string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range cmdline {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- OS probes (Windows) ---------------------------------------------------------------------

// pidForPortWindows returns the PID owning the LISTENING socket on port, or 0 when none. It uses the
// hiddenCmd builds a command that spawns WITHOUT a console window.
//
// Every spawn in this file is a short-lived Windows query (powershell.exe for
// Get-NetTCPConnection / Win32_Process, nvidia-smi for per-process VRAM). The daemon is linked
// -H=windowsgui and therefore owns no console, so a plain exec would make Windows allocate a
// fresh console window per call — visible on screen and closable by anyone. Attach polls these,
// so it is a repeating flash, not a one-off.
func hiddenCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	winproc.Hide(cmd)
	return cmd
}

// same Get-NetTCPConnection call the project's launcher script uses, so both agree on ownership.
func pidForPortWindows(ctx context.Context, port int) (int, error) {
	ps := fmt.Sprintf(
		"(Get-NetTCPConnection -State Listen -LocalPort %d -ErrorAction SilentlyContinue | Select-Object -First 1).OwningProcess",
		port)
	out, err := hiddenCmd(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		return 0, fmt.Errorf("Get-NetTCPConnection failed: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

// commandLineWindows reads a process's full command line via CIM.
func commandLineWindows(ctx context.Context, pid int) (string, error) {
	ps := fmt.Sprintf(
		"(Get-CimInstance Win32_Process -Filter \"ProcessId=%d\" -ErrorAction SilentlyContinue).CommandLine", pid)
	out, err := hiddenCmd(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		return "", fmt.Errorf("Win32_Process query failed: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("PID %d has no readable command line (process gone, or access denied)", pid)
	}
	return s, nil
}

// computeAppRe matches one `pid, used_memory` row of nvidia-smi --query-compute-apps CSV output.
var computeAppRe = regexp.MustCompile(`^\s*(\d+)\s*,\s*(\d+)`)

// processVRAMMiB reads the ACTUAL per-process VRAM for pid from nvidia-smi. This is the measurement
// that lets an adopted instance carry driver truth instead of an estimate — and being per-process, it
// stays correct while other models are resident, unlike a whole-GPU delta (the concurrency hazard
// recorded in KNOWN-LIMITATIONS #11).
func processVRAMMiB(ctx context.Context, pid int) (int, error) {
	out, err := hiddenCmd(ctx, "nvidia-smi",
		"--query-compute-apps=pid,used_gpu_memory", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, fmt.Errorf("nvidia-smi compute-apps query failed: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		m := computeAppRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if p, _ := strconv.Atoi(m[1]); p == pid {
			mib, cerr := strconv.Atoi(m[2])
			if cerr != nil {
				return 0, cerr
			}
			return mib, nil
		}
	}
	return 0, fmt.Errorf("PID %d is not using GPU memory according to nvidia-smi", pid)
}
