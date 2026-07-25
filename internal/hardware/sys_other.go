//go:build !windows

package hardware

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// otherSystemProbe provides the portable non-Windows implementation. Linux uses the
// kernel's procfs/statfs interfaces, keeping the control plane free of CUDA/NVML.
type otherSystemProbe struct{}

func newSystemProbe() SystemProbe { return otherSystemProbe{} }

func (otherSystemProbe) CPU(ctx context.Context) (CPU, error) {
	// runtime.NumCPU gives logical processors; physical-core detection is deferred.
	n := runtime.NumCPU()
	return CPU{Model: runtime.GOARCH + " CPU", Cores: n, Threads: n, UtilizationPct: -1}, nil
}

func (otherSystemProbe) RAM(ctx context.Context) (RAM, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return RAM{}, err
	}
	var total, available uint64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, e := strconv.ParseUint(f[1], 10, 64)
		if e != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			available = v
		}
	}
	if total == 0 {
		return RAM{}, os.ErrInvalid
	}
	return RAM{TotalMiB: int(total / 1024), FreeMiB: int(available / 1024), UsedMiB: int((total - available) / 1024)}, nil
}

func (otherSystemProbe) Disks(ctx context.Context, paths []string) ([]Disk, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	seen := map[string]bool{}
	var out []Disk
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		var st unix.Statfs_t
		if err := unix.Statfs(abs, &st); err != nil {
			continue
		}
		b := uint64(st.Bsize)
		total := float64(st.Blocks*b) / (1024 * 1024 * 1024)
		free := float64(st.Bavail*b) / (1024 * 1024 * 1024)
		out = append(out, Disk{Path: abs, TotalGiB: round2(total), FreeGiB: round2(free), UsedGiB: round2(total - free)})
	}
	return out, nil
}

// hideWindow is a no-op off Windows (no console-flash concern).
func hideWindow(cmd *exec.Cmd) {}
