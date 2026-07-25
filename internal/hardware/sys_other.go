//go:build !windows

package hardware

import (
	"context"
	"os/exec"
	"runtime"
)

// otherSystemProbe is the non-Windows fallback so the package COMPILES on every platform
// (the reference box is Windows-first, but Linux support lands later behind the same
// interfaces). It returns best-effort/zero values rather than syscalls: real Linux CPU/RAM/
// disk probing (sysconf, /proc/meminfo, statfs) is a later task. UtilizationPct is -1 and
// RAM/Disk are zero-valued; the GPU path (nvidia-smi) still works identically on Linux.
//
// This stub is intentionally minimal — it must compile and not panic, not be complete.
type otherSystemProbe struct{}

func newSystemProbe() SystemProbe { return otherSystemProbe{} }

func (otherSystemProbe) CPU(ctx context.Context) (CPU, error) {
	// runtime.NumCPU gives logical processors; physical-core detection is deferred.
	n := runtime.NumCPU()
	return CPU{Model: runtime.GOARCH + " CPU", Cores: n, Threads: n, UtilizationPct: -1}, nil
}

func (otherSystemProbe) RAM(ctx context.Context) (RAM, error) {
	// Real /proc/meminfo parsing is a later Linux task; return zero so callers don't crash.
	return RAM{}, nil
}

func (otherSystemProbe) Disks(ctx context.Context, paths []string) ([]Disk, error) {
	return []Disk{}, nil
}

// hideWindow is a no-op off Windows (no console-flash concern).
func hideWindow(cmd *exec.Cmd) {}
