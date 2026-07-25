package hardware

import (
	"context"
	"time"
)

// FakeProbe is an exported test double implementing Probe. Other packages (core, api,
// compat, residency) use it to test hardware-dependent logic without a real GPU. It returns
// a fixed snapshot (or a per-call function, if set) and records how many times Snapshot was
// called — useful for residency tests that assert free VRAM is re-read after each eviction.
//
// Zero value is usable: it returns DefaultFakeSnapshot(). Set Snap for a custom fixed
// snapshot, SnapFunc for dynamic behavior, or Err to simulate a probe failure.
type FakeProbe struct {
	// Snap, if set (non-zero CapturedAt OR non-nil GPUs), is returned by Snapshot.
	Snap Snapshot
	// SnapFunc, if set, takes precedence over Snap and lets a test vary the snapshot per
	// call (e.g. free VRAM increasing after an eviction).
	SnapFunc func(ctx context.Context, call int) (Snapshot, error)
	// Err, if set and SnapFunc is nil, is returned by Snapshot.
	Err error

	// Calls counts Snapshot invocations (for re-measure assertions). Not concurrency-safe;
	// tests drive it single-threaded, which is the intended use.
	Calls int
}

// Snapshot implements Probe.
func (f *FakeProbe) Snapshot(ctx context.Context) (Snapshot, error) {
	f.Calls++
	if f.SnapFunc != nil {
		return f.SnapFunc(ctx, f.Calls)
	}
	if f.Err != nil {
		return Snapshot{}, f.Err
	}
	if f.Snap.GPUs != nil || !f.Snap.CapturedAt.IsZero() {
		return f.Snap, nil
	}
	return DefaultFakeSnapshot(), nil
}

// DefaultFakeSnapshot returns a plausible snapshot modeled on the reference box (RTX 4060 Ti
// 16 GB, CC 8.9, 16 GB RAM). Handy as a baseline in other packages' tests.
func DefaultFakeSnapshot() Snapshot {
	return Snapshot{
		GPUs: []GPU{{
			Index:             0,
			Name:              "NVIDIA GeForce RTX 4060 Ti",
			VRAMTotalMiB:      16380,
			VRAMFreeMiB:       15200,
			VRAMUsedMiB:       1180,
			DriverVersion:     "576.40",
			ComputeCapability: "8.9",
			UtilizationPct:    3,
			TemperatureC:      45,
			PowerDrawW:        14.5,
		}},
		CPU: CPU{
			Model:          "Fake CPU 8-Core",
			Cores:          8,
			Threads:        16,
			UtilizationPct: -1,
		},
		RAM: RAM{TotalMiB: 16000, FreeMiB: 8000, UsedMiB: 8000},
		Disks: []Disk{
			{Path: `K:\`, TotalGiB: 930.0, FreeGiB: 25.0, UsedGiB: 905.0},
		},
		CapturedAt: time.Unix(0, 0).UTC(),
	}
}
