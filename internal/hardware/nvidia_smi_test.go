package hardware

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The canonical healthy single-GPU line from the reference box (P0.1 field validation),
// 9-column name-first form: name, total, free, used, driver, cc, util, temp, power.
const canned4060Ti = "NVIDIA GeForce RTX 4060 Ti, 16380, 13293, 2817, 576.40, 8.9, 1, 49, 14.52"

func TestParseNvidiaSmiCSV_HealthySingleGPU(t *testing.T) {
	gpus := parseNvidiaSmiCSV([]byte(canned4060Ti))
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}
	g := gpus[0]
	if g.Index != 0 {
		t.Errorf("index: want 0 (synthesized), got %d", g.Index)
	}
	if g.Name != "NVIDIA GeForce RTX 4060 Ti" {
		t.Errorf("name: got %q", g.Name)
	}
	if g.VRAMTotalMiB != 16380 || g.VRAMFreeMiB != 13293 || g.VRAMUsedMiB != 2817 {
		t.Errorf("vram: got total=%d free=%d used=%d", g.VRAMTotalMiB, g.VRAMFreeMiB, g.VRAMUsedMiB)
	}
	if g.DriverVersion != "576.40" {
		t.Errorf("driver: got %q", g.DriverVersion)
	}
	if g.ComputeCapability != "8.9" {
		t.Errorf("compute_cap: want 8.9 (verified on driver 576.40), got %q", g.ComputeCapability)
	}
	if g.UtilizationPct != 1 {
		t.Errorf("util: want 1, got %v", g.UtilizationPct)
	}
	if g.TemperatureC != 49 {
		t.Errorf("temp: want 49, got %v", g.TemperatureC)
	}
	if g.PowerDrawW != 14.52 {
		t.Errorf("power: want 14.52, got %v", g.PowerDrawW)
	}
}

func TestParseNvidiaSmiCSV_IndexFirst10Col(t *testing.T) {
	// The format execNvidiaSmi actually requests (index prepended).
	line := "0, NVIDIA GeForce RTX 4060 Ti, 16380, 13288, 2822, 576.40, 8.9, 0, 55, 14.82\n"
	gpus := parseNvidiaSmiCSV([]byte(line))
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}
	g := gpus[0]
	if g.Index != 0 || g.Name != "NVIDIA GeForce RTX 4060 Ti" || g.VRAMTotalMiB != 16380 {
		t.Errorf("10-col parse wrong: %+v", g)
	}
	if g.UtilizationPct != 0 {
		t.Errorf("idle util: want 0, got %v", g.UtilizationPct)
	}
}

func TestParseNvidiaSmiCSV_MultiGPU(t *testing.T) {
	// Two GPUs, index-first, second has non-zero index.
	raw := "0, NVIDIA GeForce RTX 4060 Ti, 16380, 13000, 3380, 576.40, 8.9, 12, 50, 60.5\n" +
		"1, NVIDIA GeForce RTX 3090, 24576, 20000, 4576, 576.40, 8.6, 30, 62, 210.0\n"
	gpus := parseNvidiaSmiCSV([]byte(raw))
	if len(gpus) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(gpus))
	}
	if gpus[0].Index != 0 || gpus[1].Index != 1 {
		t.Errorf("indices: got %d, %d", gpus[0].Index, gpus[1].Index)
	}
	if gpus[1].Name != "NVIDIA GeForce RTX 3090" || gpus[1].VRAMTotalMiB != 24576 {
		t.Errorf("second GPU wrong: %+v", gpus[1])
	}
	if gpus[1].ComputeCapability != "8.6" {
		t.Errorf("second GPU cc: got %q", gpus[1].ComputeCapability)
	}
}

func TestParseNvidiaSmiCSV_MultiGPU_NameFirst(t *testing.T) {
	// Two GPUs, 9-column name-first: indices synthesized from row order.
	raw := canned4060Ti + "\n" +
		"NVIDIA GeForce RTX 3090, 24576, 20000, 4576, 576.40, 8.6, 30, 62, 210.0\n"
	gpus := parseNvidiaSmiCSV([]byte(raw))
	if len(gpus) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(gpus))
	}
	if gpus[0].Index != 0 || gpus[1].Index != 1 {
		t.Errorf("synthesized indices: got %d, %d", gpus[0].Index, gpus[1].Index)
	}
}

func TestParseNvidiaSmiCSV_Garbage(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"whitespace only":  "   \n\t\n",
		"too few columns":  "NVIDIA GeForce RTX 4060 Ti, 16380, 13293\n",
		"non-numeric junk": "this is not, a valid, nvidia-smi, line at all, really\n",
		"html error page":  "<html><body>500 Internal Server Error</body></html>\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			gpus := parseNvidiaSmiCSV([]byte(raw))
			if len(gpus) != 0 {
				t.Errorf("garbage %q: want 0 GPUs, got %d (%+v)", name, len(gpus), gpus)
			}
		})
	}
}

func TestParseNvidiaSmiCSV_NASentinels(t *testing.T) {
	// Some fields report [N/A] (e.g. laptop dGPU power, or util on certain drivers).
	line := "0, NVIDIA GPU, 8192, 4096, 4096, 550.00, 7.5, [N/A], [N/A], [N/A]\n"
	gpus := parseNvidiaSmiCSV([]byte(line))
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}
	g := gpus[0]
	if g.UtilizationPct != -1 {
		t.Errorf("N/A util: want -1 sentinel, got %v", g.UtilizationPct)
	}
	if g.TemperatureC != -1 {
		t.Errorf("N/A temp: want -1 sentinel, got %v", g.TemperatureC)
	}
	if g.PowerDrawW != 0 {
		t.Errorf("N/A power: want 0, got %v", g.PowerDrawW)
	}
	// The good fields must still parse.
	if g.VRAMTotalMiB != 8192 || g.ComputeCapability != "7.5" {
		t.Errorf("good fields poisoned by N/A: %+v", g)
	}
}

func TestParseNvidiaSmiCSV_MixedValidAndMalformed(t *testing.T) {
	// A malformed middle line must be skipped without dropping the valid ones.
	raw := canned4060Ti + "\n" +
		"garbage line, too, few\n" +
		"NVIDIA GeForce RTX 3090, 24576, 20000, 4576, 576.40, 8.6, 30, 62, 210.0\n"
	gpus := parseNvidiaSmiCSV([]byte(raw))
	if len(gpus) != 2 {
		t.Fatalf("want 2 valid GPUs (malformed skipped), got %d", len(gpus))
	}
}

func TestNvidiaSmiProbe_GPUs_WithFakeRunner(t *testing.T) {
	p := NewNvidiaSmiProbe(withRunner(func(ctx context.Context, binary string, timeout time.Duration) ([]byte, error) {
		return []byte(canned4060Ti), nil
	}))
	gpus, err := p.GPUs(context.Background())
	if err != nil {
		t.Fatalf("GPUs: %v", err)
	}
	if len(gpus) != 1 || gpus[0].VRAMTotalMiB != 16380 {
		t.Errorf("unexpected: %+v", gpus)
	}
}

func TestNvidiaSmiProbe_GPUs_MissingBinary(t *testing.T) {
	p := NewNvidiaSmiProbe(withRunner(func(ctx context.Context, binary string, timeout time.Duration) ([]byte, error) {
		return nil, ErrNvidiaSmiNotFound
	}))
	_, err := p.GPUs(context.Background())
	if !errors.Is(err, ErrNvidiaSmiNotFound) {
		t.Fatalf("want ErrNvidiaSmiNotFound, got %v", err)
	}
}

// Composite Snapshot must degrade to zero GPUs (not error) when nvidia-smi is missing.
func TestComposite_MissingSmi_ZeroGPUsNotError(t *testing.T) {
	gpu := NewNvidiaSmiProbe(withRunner(func(ctx context.Context, binary string, timeout time.Duration) ([]byte, error) {
		return nil, ErrNvidiaSmiNotFound
	}))
	sys := fakeSys{cpu: CPU{Model: "x", Cores: 4, Threads: 8, UtilizationPct: -1}, ram: RAM{TotalMiB: 16000, FreeMiB: 8000, UsedMiB: 8000}}
	c := newComposite(gpu, sys, nil)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot should not fail on missing smi: %v", err)
	}
	if len(snap.GPUs) != 0 {
		t.Errorf("want 0 GPUs, got %d", len(snap.GPUs))
	}
	if snap.CPU.Cores != 4 || snap.RAM.TotalMiB != 16000 {
		t.Errorf("CPU/RAM should still be populated: %+v / %+v", snap.CPU, snap.RAM)
	}
}

// fakeSys is a SystemProbe stub for composite tests.
type fakeSys struct {
	cpu   CPU
	ram   RAM
	disks []Disk
	err   error
}

func (f fakeSys) CPU(ctx context.Context) (CPU, error) { return f.cpu, f.err }
func (f fakeSys) RAM(ctx context.Context) (RAM, error) { return f.ram, f.err }
func (f fakeSys) Disks(ctx context.Context, p []string) ([]Disk, error) {
	return f.disks, f.err
}
