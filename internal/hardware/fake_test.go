package hardware

import (
	"context"
	"errors"
	"testing"
)

func TestFakeProbe_Default(t *testing.T) {
	var f FakeProbe // zero value
	snap, err := f.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.GPUs) != 1 || snap.GPUs[0].Name != "NVIDIA GeForce RTX 4060 Ti" {
		t.Errorf("default fake GPU wrong: %+v", snap.GPUs)
	}
	if f.Calls != 1 {
		t.Errorf("Calls: want 1, got %d", f.Calls)
	}
}

func TestFakeProbe_CustomSnap(t *testing.T) {
	custom := Snapshot{GPUs: []GPU{{Index: 0, Name: "test", VRAMFreeMiB: 42}}}
	f := FakeProbe{Snap: custom}
	snap, _ := f.Snapshot(context.Background())
	if snap.GPUs[0].VRAMFreeMiB != 42 {
		t.Errorf("custom snap not returned: %+v", snap)
	}
}

func TestFakeProbe_Err(t *testing.T) {
	sentinel := errors.New("boom")
	f := FakeProbe{Err: sentinel}
	_, err := f.Snapshot(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel err, got %v", err)
	}
}

func TestFakeProbe_SnapFunc_CallCount(t *testing.T) {
	// Simulates free VRAM rising after an eviction (residency re-measure use case).
	f := FakeProbe{
		SnapFunc: func(ctx context.Context, call int) (Snapshot, error) {
			s := DefaultFakeSnapshot()
			s.GPUs[0].VRAMFreeMiB = 1000 * call
			return s, nil
		},
	}
	for i := 1; i <= 3; i++ {
		snap, _ := f.Snapshot(context.Background())
		if snap.GPUs[0].VRAMFreeMiB != 1000*i {
			t.Errorf("call %d: want free %d, got %d", i, 1000*i, snap.GPUs[0].VRAMFreeMiB)
		}
	}
	if f.Calls != 3 {
		t.Errorf("Calls: want 3, got %d", f.Calls)
	}
}

// FakeProbe must satisfy the Probe interface.
var _ Probe = (*FakeProbe)(nil)
