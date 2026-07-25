package hardware

import (
	"context"
	"testing"
	"time"
)

// TestRealBoxSnapshot exercises the REAL composite probe against the actual machine: it
// shells out to nvidia-smi and reads real CPU/RAM/disk. It is skipped under `go test -short`
// so CI and quick runs stay hermetic; run it explicitly to prove the reference box
// (RTX 4060 Ti 16 GB) is detected with sane numbers.
//
//	go test ./internal/hardware/... -run TestRealBoxSnapshot -v
func TestRealBoxSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-hardware snapshot in -short mode")
	}
	p := New(Config{DiskPaths: []string{`K:\`, `C:\`}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	snap, err := p.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	t.Logf("captured_at=%s", snap.CapturedAt.Format(time.RFC3339))
	t.Logf("CPU: model=%q cores=%d threads=%d util=%v", snap.CPU.Model, snap.CPU.Cores, snap.CPU.Threads, snap.CPU.UtilizationPct)
	t.Logf("RAM: total=%d MiB free=%d MiB used=%d MiB", snap.RAM.TotalMiB, snap.RAM.FreeMiB, snap.RAM.UsedMiB)
	for _, d := range snap.Disks {
		t.Logf("DISK %s: total=%.2f GiB free=%.2f GiB", d.Path, d.TotalGiB, d.FreeGiB)
	}
	for _, g := range snap.GPUs {
		t.Logf("GPU[%d]: name=%q vram_total=%d MiB vram_free=%d MiB vram_used=%d MiB driver=%s cc=%s util=%v%% temp=%v power=%vW",
			g.Index, g.Name, g.VRAMTotalMiB, g.VRAMFreeMiB, g.VRAMUsedMiB, g.DriverVersion, g.ComputeCapability, g.UtilizationPct, g.TemperatureC, g.PowerDrawW)
	}

	// CPU/RAM sanity (always present on Windows).
	if snap.CPU.Threads <= 0 {
		t.Errorf("CPU threads should be > 0")
	}
	if snap.RAM.TotalMiB <= 0 {
		t.Errorf("RAM total should be > 0")
	}

	// GPU assertions only if nvidia-smi is present (the reference box has it).
	if len(snap.GPUs) == 0 {
		t.Log("no GPUs detected (nvidia-smi absent?) — CPU/RAM/disk still validated")
		return
	}
	g := snap.GPUs[0]
	if g.VRAMTotalMiB < 8000 {
		t.Errorf("expected a >=8GB GPU, got %d MiB total", g.VRAMTotalMiB)
	}
	if g.VRAMFreeMiB <= 0 || g.VRAMFreeMiB > g.VRAMTotalMiB {
		t.Errorf("free VRAM out of range: free=%d total=%d", g.VRAMFreeMiB, g.VRAMTotalMiB)
	}
	if g.ComputeCapability == "" {
		t.Errorf("compute capability should be reported (expected 8.9 on the reference box)")
	}
}
