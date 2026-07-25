//go:build windows

package hardware

import (
	"context"
	"testing"
)

func TestWinRAM_PlausibleNonzero(t *testing.T) {
	sp := newSystemProbe()
	ram, err := sp.RAM(context.Background())
	if err != nil {
		t.Fatalf("RAM: %v", err)
	}
	if ram.TotalMiB <= 0 {
		t.Fatalf("total RAM should be > 0, got %d MiB", ram.TotalMiB)
	}
	if ram.FreeMiB < 0 || ram.FreeMiB > ram.TotalMiB {
		t.Errorf("free RAM out of range: free=%d total=%d", ram.FreeMiB, ram.TotalMiB)
	}
	if ram.UsedMiB != ram.TotalMiB-ram.FreeMiB {
		t.Errorf("used != total-free: used=%d total=%d free=%d", ram.UsedMiB, ram.TotalMiB, ram.FreeMiB)
	}
	// Sanity: a Windows box has at least ~1 GiB total.
	if ram.TotalMiB < 1024 {
		t.Errorf("implausibly small total RAM: %d MiB", ram.TotalMiB)
	}
	t.Logf("RAM total=%d MiB free=%d MiB used=%d MiB", ram.TotalMiB, ram.FreeMiB, ram.UsedMiB)
}

func TestWinDisks_PlausibleNonzero(t *testing.T) {
	sp := newSystemProbe()
	disks, err := sp.Disks(context.Background(), []string{`C:\`})
	if err != nil {
		t.Fatalf("Disks: %v", err)
	}
	if len(disks) == 0 {
		t.Fatalf("want at least one disk for C:")
	}
	d := disks[0]
	if d.TotalGiB <= 0 {
		t.Errorf("total disk should be > 0, got %.2f GiB", d.TotalGiB)
	}
	if d.FreeGiB < 0 || d.FreeGiB > d.TotalGiB {
		t.Errorf("free disk out of range: free=%.2f total=%.2f", d.FreeGiB, d.TotalGiB)
	}
	t.Logf("disk %s total=%.2f GiB free=%.2f GiB", d.Path, d.TotalGiB, d.FreeGiB)
}

func TestWinDisks_DedupSameVolume(t *testing.T) {
	sp := newSystemProbe()
	// Two paths on the same drive must collapse to one Disk entry.
	disks, err := sp.Disks(context.Background(), []string{`C:\Windows`, `C:\Users`})
	if err != nil {
		t.Fatalf("Disks: %v", err)
	}
	if len(disks) != 1 {
		t.Errorf("want 1 deduped disk for C:, got %d: %+v", len(disks), disks)
	}
}

func TestWinCPU_ModelAndCounts(t *testing.T) {
	sp := newSystemProbe()
	cpu, err := sp.CPU(context.Background())
	if err != nil {
		t.Fatalf("CPU: %v", err)
	}
	if cpu.Threads <= 0 {
		t.Errorf("threads should be > 0, got %d", cpu.Threads)
	}
	if cpu.Cores <= 0 || cpu.Cores > cpu.Threads {
		t.Errorf("cores out of range: cores=%d threads=%d", cpu.Cores, cpu.Threads)
	}
	if cpu.UtilizationPct != -1 {
		t.Errorf("v1 CPU util is deliberately -1, got %v", cpu.UtilizationPct)
	}
	// Model may be empty on exotic hardware but on this box should be non-empty.
	t.Logf("CPU model=%q cores=%d threads=%d", cpu.Model, cpu.Cores, cpu.Threads)
}
