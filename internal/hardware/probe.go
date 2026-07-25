package hardware

import (
	"context"
	"errors"
	"sync"
	"time"
)

// SystemProbe reads CPU/RAM/disk. It is platform-specific (see sys_windows.go and
// sys_other.go) and injected into the composite probe so the GPU path and the OS path are
// independently testable. Disks are probed for the given paths (typically the configured
// model directories); an empty paths slice means "probe sensible defaults for this OS".
type SystemProbe interface {
	CPU(ctx context.Context) (CPU, error)
	RAM(ctx context.Context) (RAM, error)
	Disks(ctx context.Context, paths []string) ([]Disk, error)
}

// probe is the composite Probe: it combines a GPU probe (NvidiaSmiProbe) and a SystemProbe
// (OS-specific) into one Snapshot, and caches the last good snapshot with its timestamp.
// Polling cadence is the caller's responsibility (the metrics loop calls Snapshot at
// Hardware.PollIntervalSeconds); this type only remembers the most recent success so a
// transient GPU-probe hiccup can serve slightly stale data rather than an error.
type probe struct {
	gpu       *NvidiaSmiProbe
	sys       SystemProbe
	diskPaths []string

	mu       sync.RWMutex
	lastGood *Snapshot
}

// Config configures the composite probe.
type Config struct {
	// DiskPaths are the volumes/directories to report free space for (usually the
	// configured model dirs). Empty → OS defaults.
	DiskPaths []string
	// SmiOptions are passed to the NvidiaSmiProbe (binary override, timeout).
	SmiOptions []NvidiaSmiOption
}

// New builds the default composite Probe for this OS: nvidia-smi for GPUs plus the
// platform SystemProbe for CPU/RAM/disk. It is the constructor wiring code (core.Deps)
// should call.
func New(cfg Config) Probe {
	return &probe{
		gpu:       NewNvidiaSmiProbe(cfg.SmiOptions...),
		sys:       newSystemProbe(),
		diskPaths: cfg.DiskPaths,
	}
}

// newComposite builds a composite probe from explicit parts. Exposed within the package for
// tests that want to inject a fake SystemProbe or GPU runner.
func newComposite(gpu *NvidiaSmiProbe, sys SystemProbe, diskPaths []string) *probe {
	return &probe{gpu: gpu, sys: sys, diskPaths: diskPaths}
}

// Snapshot assembles a full hardware snapshot. GPU-probe failure is non-fatal: a missing
// nvidia-smi (ErrNvidiaSmiNotFound) yields zero GPUs; any other GPU error also degrades to
// zero GPUs rather than failing the whole snapshot (the dashboard still wants CPU/RAM/disk).
// A genuine failure to read CPU/RAM/disk returns an error — but if a prior good snapshot
// exists, it is returned instead so the caller keeps serving data.
func (p *probe) Snapshot(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{CapturedAt: time.Now().UTC()}

	// GPUs: never fatal.
	if p.gpu != nil {
		gpus, err := p.gpu.GPUs(ctx)
		if err != nil {
			// ErrNvidiaSmiNotFound and any other GPU error → no GPUs, keep going.
			snap.GPUs = []GPU{}
		} else {
			snap.GPUs = gpus
		}
	}
	if snap.GPUs == nil {
		snap.GPUs = []GPU{}
	}

	// System info: a failure here is the only thing that can error the snapshot, and even
	// then we prefer a cached good snapshot to nothing.
	var sysErr error
	if p.sys != nil {
		if cpu, err := p.sys.CPU(ctx); err == nil {
			snap.CPU = cpu
		} else {
			sysErr = errors.Join(sysErr, err)
			snap.CPU = CPU{UtilizationPct: -1}
		}
		if ram, err := p.sys.RAM(ctx); err == nil {
			snap.RAM = ram
		} else {
			sysErr = errors.Join(sysErr, err)
		}
		if disks, err := p.sys.Disks(ctx, p.diskPaths); err == nil {
			snap.Disks = disks
		} else {
			sysErr = errors.Join(sysErr, err)
		}
	}
	if snap.Disks == nil {
		snap.Disks = []Disk{}
	}

	if sysErr != nil {
		if cached := p.getLastGood(); cached != nil {
			return *cached, nil
		}
		return snap, sysErr
	}

	p.setLastGood(snap)
	return snap, nil
}

func (p *probe) getLastGood() *Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.lastGood == nil {
		return nil
	}
	cp := *p.lastGood
	return &cp
}

func (p *probe) setLastGood(s Snapshot) {
	p.mu.Lock()
	p.lastGood = &s
	p.mu.Unlock()
}
