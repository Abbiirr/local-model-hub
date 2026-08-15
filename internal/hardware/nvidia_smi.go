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

// nvidiaSmiExtendedFields is the PREFERRED query: the 10 base columns above plus 11 extended
// telemetry columns (clocks, fan, PCIe link, power limits, throttle reasons). It is tried
// first; execNvidiaSmi falls back to the base field sets if a driver rejects any of these.
//
// The extra fields cost nothing — same single invocation — and answer "why is it SLOW", which
// VRAM numbers alone cannot. Column ORDER here must match parseNvidiaSmiCSV's 21-column
// branch; change the two together or the parse silently misaligns.
const nvidiaSmiExtendedFields = nvidiaSmiQueryFields +
	",clocks.sm,clocks.mem,clocks.max.sm,fan.speed" +
	",pcie.link.gen.current,pcie.link.gen.max,pcie.link.width.current,pcie.link.width.max" +
	",power.limit,power.max_limit,clocks_throttle_reasons.active"

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

	run := func(fields string) ([]byte, error) {
		cmd := exec.CommandContext(cctx, binary,
			"--query-gpu="+fields,
			"--format=csv,noheader,nounits",
		)
		hideWindow(cmd)
		return cmd.Output()
	}
	// Degrade one step at a time: extended → base(+compute_cap) → minimal. Each fallback drops
	// only the fields a given driver refused, so an old driver loses telemetry detail but never
	// the core VRAM numbers the fit verdict depends on.
	out, err := run(nvidiaSmiExtendedFields)
	if err != nil && cctx.Err() == nil {
		out, err = run(nvidiaSmiQueryFields)
	}
	if err != nil && cctx.Err() == nil {
		// compute_cap is absent on older supported drivers. A missing optional
		// capability must not make a healthy NVIDIA box look like it has no GPU.
		out, err = run("name,memory.total,memory.free,memory.used,driver_version,utilization.gpu,temperature.gpu,power.draw")
	}
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("nvidia-smi timed out after %s", timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}
	return out, nil
}

// Processes lists processes currently holding a CUDA context on this box.
//
// Returns (nil, err) when nvidia-smi is missing or the query fails; callers should treat that
// as "unknown", not as "nothing is using the GPU". Note UsedMiB is -1 on Windows regardless of
// success — see GPUProcess.
func (p *NvidiaSmiProbe) Processes(ctx context.Context) ([]GPUProcess, error) {
	binary := p.binary
	if binary == "" {
		binary = "nvidia-smi"
	}
	timeout := p.timeout
	if timeout == 0 {
		timeout = defaultSmiTimeout
	}
	raw, err := execNvidiaSmiComputeApps(ctx, binary, timeout)
	if err != nil {
		return nil, err
	}
	return parseComputeApps(raw), nil
}

// execNvidiaSmiComputeApps lists processes holding a CUDA context. Separate invocation from
// the --query-gpu call because nvidia-smi cannot combine the two query modes.
//
// Failure is deliberately soft: the caller drops the list and keeps the rest of the snapshot.
// Knowing GPU memory is 12 GiB used is useful even when we cannot say who is using it.
func execNvidiaSmiComputeApps(ctx context.Context, binary string, timeout time.Duration) ([]byte, error) {
	if _, err := exec.LookPath(binary); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNvidiaSmiNotFound, err)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, binary,
		"--query-compute-apps=pid,used_memory,name",
		"--format=csv,noheader,nounits",
	)
	hideWindow(cmd)
	out, err := cmd.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("nvidia-smi compute-apps timed out after %s", timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi compute-apps failed: %w", err)
	}
	return out, nil
}

// parseComputeApps parses `--query-compute-apps=pid,used_memory,name` CSV into GPUProcess.
// Pure function, unit-testable without a GPU.
//
// used_memory is "[N/A]" on every row under Windows WDDM (see GPUProcess), so UsedMiB is -1
// far more often than not. Rows whose pid will not parse are skipped; a row with a valid pid
// but an unreadable name (Windows returns "[Insufficient Permissions]" for other users'
// processes) is KEPT with an empty name, because the pid alone is still actionable.
func parseComputeApps(raw []byte) []GPUProcess {
	var procs []GPUProcess
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		cols := splitCSV(line)
		if len(cols) < 3 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(cols[0]))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(cols[2])
		if isNA(name) {
			name = ""
		}
		procs = append(procs, GPUProcess{
			PID:     pid,
			Name:    name,
			UsedMiB: atoiOr(cols[1], -1),
		})
	}
	return procs
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
			g        GPU
			hasIdx   bool
			extended bool
		)
		// Extended telemetry defaults to -1 ("nvidia-smi did not report this"), NOT 0. A 0 fan
		// speed or 0 MHz clock is a legitimate reading; conflating "unknown" with "zero" would
		// make the UI state things that are not true.
		g.ClockSMMHz, g.ClockMemMHz, g.ClockSMMaxMHz = -1, -1, -1
		g.FanPct = -1
		g.PCIeGen, g.PCIeGenMax, g.PCIeWidth, g.PCIeWidthMax = -1, -1, -1, -1
		g.PowerLimitW, g.PowerMaxLimitW = -1, -1
		switch len(cols) {
		case 21: // base 10 + 11 extended telemetry columns
			hasIdx, extended = true, true
		case 10:
			hasIdx = true
		case 9:
			hasIdx = false
		case 8: // compatibility retry without the unsupported compute_cap field
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
		if len(cols) == 8 {
			g.UtilizationPct = atofOrNA(cols[off+5], -1)
			g.TemperatureC = atofOrNA(cols[off+6], -1)
			g.PowerDrawW = atofOrNA(cols[off+7], 0)
		} else {
			g.ComputeCapability = strings.TrimSpace(cols[off+5])
			g.UtilizationPct = atofOrNA(cols[off+6], -1)
			g.TemperatureC = atofOrNA(cols[off+7], -1)
			g.PowerDrawW = atofOrNA(cols[off+8], 0)
		}

		if extended {
			// Columns 10..20, in nvidiaSmiExtendedFields order.
			g.ClockSMMHz = atoiOr(cols[off+9], -1)
			g.ClockMemMHz = atoiOr(cols[off+10], -1)
			g.ClockSMMaxMHz = atoiOr(cols[off+11], -1)
			g.FanPct = atoiOr(cols[off+12], -1)
			g.PCIeGen = atoiOr(cols[off+13], -1)
			g.PCIeGenMax = atoiOr(cols[off+14], -1)
			g.PCIeWidth = atoiOr(cols[off+15], -1)
			g.PCIeWidthMax = atoiOr(cols[off+16], -1)
			g.PowerLimitW = atofOrNA(cols[off+17], -1)
			g.PowerMaxLimitW = atofOrNA(cols[off+18], -1)
			g.Throttle = decodeThrottleReasons(cols[off+19])
		}

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

// isNA reports whether a nvidia-smi field is a "no value here" sentinel.
//
// nvidia-smi has several of these, not just N/A: consumer cards return "[Not Supported]" for
// fields the SKU lacks (e.g. temperature.memory on a 4060 Ti), and Windows returns
// "[Insufficient Permissions]" for process fields it will not attribute. All must degrade to
// the caller's sentinel rather than parse as 0.
func isNA(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" ||
		strings.EqualFold(s, "[N/A]") || strings.EqualFold(s, "N/A") ||
		strings.EqualFold(s, "[Not Supported]") || strings.EqualFold(s, "Not Supported") ||
		strings.EqualFold(s, "[Insufficient Permissions]")
}

// throttleReasonNames maps clocks_throttle_reasons.active bits to short readable labels.
// Source: nvidia-smi --help-query-gpu (bitmask semantics are stable across driver branches).
var throttleReasonNames = []struct {
	bit  uint64
	name string
}{
	{0x0000000000000001, "idle"},
	{0x0000000000000002, "app_clocks_setting"},
	{0x0000000000000004, "sw_power_cap"},
	{0x0000000000000008, "hw_slowdown"},
	{0x0000000000000010, "sync_boost"},
	{0x0000000000000020, "sw_thermal_slowdown"},
	{0x0000000000000040, "hw_thermal_slowdown"},
	{0x0000000000000080, "hw_power_brake_slowdown"},
	{0x0000000000000100, "display_clock_setting"},
}

// decodeThrottleReasons turns the hex bitmask nvidia-smi reports (e.g. "0x0000000000000001")
// into comma-joined labels. It exists so the UI can say WHY the GPU is slow instead of showing
// an opaque hex value. Returns "" for N/A, zero, or an unparseable value — meaning
// "unthrottled", which is also the correct display when nothing is limiting clocks.
func decodeThrottleReasons(raw string) string {
	if isNA(raw) {
		return ""
	}
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	mask, err := strconv.ParseUint(s, 16, 64)
	if err != nil || mask == 0 {
		return ""
	}
	var out []string
	for _, r := range throttleReasonNames {
		if mask&r.bit != 0 {
			out = append(out, r.name)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, ",")
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
