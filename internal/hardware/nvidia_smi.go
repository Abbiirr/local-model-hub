package hardware

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrNvidiaSmiNotFound is returned by the low-level GPU probe when the nvidia-smi binary is
// not on PATH. Callers (the composite probe) treat this as "zero GPUs", NOT as a failure of
// the whole snapshot — a box without an NVIDIA GPU still has valid CPU/RAM/disk info.
var ErrNvidiaSmiNotFound = errors.New("nvidia-smi not found on PATH")

// nvidiaSmiQueryFields is the exact --query-gpu field list, index first so multi-GPU output
// is unambiguous. Order MUST match parseNvidiaSmiCSV's 10-column layout.
const nvidiaSmiQueryFields = "index,name,memory.total,memory.free,memory.used,driver_version,compute_cap,utilization.gpu,temperature.gpu,power.draw"

// defaultSmiTimeout bounds the nvidia-smi exec. On a healthy box it returns in tens of ms;
// a hung driver must not wedge the metrics loop or an HTTP handler.
const defaultSmiTimeout = 2 * time.Second

// NvidiaSmiProbe reads GPU state by shelling out to `nvidia-smi` in a single invocation for
// all GPU fields. It never links CUDA/NVML (PLAN.md decision 5). It caches the last good
// snapshot with a timestamp; polling cadence is the caller's job (the metrics loop), not the
// probe's.
//
// Only the GPU portion of a Snapshot is produced here; the composite probe (systemProbe)
// stitches GPU + CPU/RAM/disk together. NvidiaSmiProbe is exported and usable standalone if a
// caller wants GPUs only.
type NvidiaSmiProbe struct {
	// binary is the nvidia-smi executable name/path. Empty means look up "nvidia-smi" on
	// PATH. Overridable for tests.
	binary string
	// timeout bounds each exec. Zero means defaultSmiTimeout.
	timeout time.Duration
	// runner executes the query and returns raw CSV bytes. Injectable so parser/probe logic
	// is unit-testable without a real GPU. nil means the real exec runner.
	runner func(ctx context.Context, binary string, timeout time.Duration) ([]byte, error)
}

// NvidiaSmiOption configures a NvidiaSmiProbe.
type NvidiaSmiOption func(*NvidiaSmiProbe)

// WithBinary overrides the nvidia-smi binary path (default: "nvidia-smi" on PATH).
func WithBinary(path string) NvidiaSmiOption {
	return func(p *NvidiaSmiProbe) { p.binary = path }
}

// WithTimeout overrides the exec timeout (default 2s).
func WithTimeout(d time.Duration) NvidiaSmiOption {
	return func(p *NvidiaSmiProbe) { p.timeout = d }
}

// withRunner injects a fake command runner (tests only).
func withRunner(fn func(ctx context.Context, binary string, timeout time.Duration) ([]byte, error)) NvidiaSmiOption {
	return func(p *NvidiaSmiProbe) { p.runner = fn }
}

// NewNvidiaSmiProbe builds a GPU probe. With no options it uses nvidia-smi from PATH and a
// 2s timeout.
func NewNvidiaSmiProbe(opts ...NvidiaSmiOption) *NvidiaSmiProbe {
	p := &NvidiaSmiProbe{binary: "nvidia-smi", timeout: defaultSmiTimeout}
	for _, o := range opts {
		o(p)
	}
	if p.binary == "" {
		p.binary = "nvidia-smi"
	}
	if p.timeout <= 0 {
		p.timeout = defaultSmiTimeout
	}
	if p.runner == nil {
		p.runner = execNvidiaSmi
	}
	return p
}

// GPUs runs one nvidia-smi query and parses it. Returns ErrNvidiaSmiNotFound (wrapped) if
// the binary is absent — callers map that to "no GPUs", not a hard failure. Malformed lines
// are skipped (best-effort), not fatal.
func (p *NvidiaSmiProbe) GPUs(ctx context.Context) ([]GPU, error) {
	raw, err := p.runner(ctx, p.binary, p.timeout)
	if err != nil {
		return nil, err
	}
	return parseNvidiaSmiCSV(raw), nil
}

// execNvidiaSmi is the real runner: it invokes nvidia-smi with the query, honoring ctx and a
// hard timeout. A missing binary maps to ErrNvidiaSmiNotFound; a non-zero exit or timeout is
// returned as a descriptive error.
func execNvidiaSmi(ctx context.Context, binary string, timeout time.Duration) ([]byte, error) {
	if _, err := exec.LookPath(binary); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNvidiaSmiNotFound, err)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, binary,
		"--query-gpu="+nvidiaSmiQueryFields,
		"--format=csv,noheader,nounits",
	)
	hideWindow(cmd) // CREATE_NO_WINDOW on Windows so we don't flash a console (no-op elsewhere)

	out, err := cmd.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("nvidia-smi timed out after %s", timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}
	return out, nil
}

// parseNvidiaSmiCSV parses `nvidia-smi --query-gpu=... --format=csv,noheader,nounits` output
// into GPUs. It is a PURE function (no I/O) so it is fully unit-testable against canned
// output.
//
// It accepts two column layouts for robustness:
//   - 10 columns: index,name,memory.total,memory.free,memory.used,driver_version,
//     compute_cap,utilization.gpu,temperature.gpu,power.draw (what execNvidiaSmi requests).
//   - 9 columns: the same WITHOUT a leading index (name-first). The GPU index is then
//     synthesized from the row position. This is the historical field set and the format of
//     the canned fixtures in the P0.1 field validation.
//
// Rules: blank lines are skipped; a line whose column count is neither 9 nor 10 is skipped
// (malformed, not fatal); "[N/A]"/"N/A" numeric fields become -1 for util/temp and 0 for
// power/memory; unparseable numerics degrade to the sentinel/zero rather than dropping the
// whole GPU (a GPU with a good name+VRAM is still useful).
func parseNvidiaSmiCSV(raw []byte) []GPU {
	var gpus []GPU
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	rowIdx := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		cols := splitCSV(line)
		var (
			g      GPU
			hasIdx bool
		)
		switch len(cols) {
		case 10:
			hasIdx = true
		case 9:
			hasIdx = false
		default:
			// Malformed line: wrong number of fields. Skip, don't abort the whole parse.
			rowIdx++
			continue
		}

		off := 0
		if hasIdx {
			if idx, err := strconv.Atoi(cols[0]); err == nil {
				g.Index = idx
			} else {
				g.Index = rowIdx
			}
			off = 1
		} else {
			g.Index = rowIdx
		}

		g.Name = cols[off+0]
		g.VRAMTotalMiB = atoiOr(cols[off+1], 0)
		g.VRAMFreeMiB = atoiOr(cols[off+2], 0)
		g.VRAMUsedMiB = atoiOr(cols[off+3], 0)
		g.DriverVersion = cols[off+4]
		g.ComputeCapability = strings.TrimSpace(cols[off+5])
		g.UtilizationPct = atofOrNA(cols[off+6], -1)
		g.TemperatureC = atofOrNA(cols[off+7], -1)
		g.PowerDrawW = atofOrNA(cols[off+8], 0)

		gpus = append(gpus, g)
		rowIdx++
	}
	return gpus
}

// splitCSV splits a nvidia-smi CSV line on commas and trims each field. nvidia-smi does not
// quote or embed commas in these numeric/name fields, so a plain split is correct and avoids
// pulling in encoding/csv (and its quoting semantics) for a format that never uses them.
func splitCSV(line string) []string {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// isNA reports whether a nvidia-smi field is the "not available" sentinel.
func isNA(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || strings.EqualFold(s, "[N/A]") || strings.EqualFold(s, "N/A")
}

// atoiOr parses an int, returning def on failure or N/A.
func atoiOr(s string, def int) int {
	if isNA(s) {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// atofOrNA parses a float, returning def on failure or N/A. Used so an idle GPU reporting
// "[N/A]" power/util degrades to a sentinel instead of poisoning the number.
func atofOrNA(s string, def float64) float64 {
	if isNA(s) {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return f
}
