package llamacpp

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/db"
	"github.com/localhub/localhub/internal/instances"
)

// scriptedVRAM returns a sequence of free-VRAM readings, advancing one per call and holding the
// last value. Models VRAM rising after a stop (reclaim) or dropping during load.
type scriptedVRAM struct {
	mu  sync.Mutex
	seq []int
	i   int
}

func (s *scriptedVRAM) FreeVRAMMiB(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.seq[s.i]
	if s.i < len(s.seq)-1 {
		s.i++
	}
	return v, nil
}

func newRunnerForTest(t *testing.T, vram VRAMProbe) (*Runner, *instances.Store) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "run.db")
	d, err := db.Open(p)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	d.Exec(`INSERT INTO models (id, display_name, source, format) VALUES ('m1','M1','local_path','gguf')`)
	d.Exec(`INSERT OR IGNORE INTO engines (name, installed) VALUES ('llamacpp', 1)`)
	store := instances.NewStore(d.DB)
	pool := instances.NewPortPool(d.DB, 40100, 40105)
	r := NewRunner(store, pool, vram, RunnerConfig{
		StopVerifyTimeout: 2 * time.Second,
		PeakSampleWindow:  200 * time.Millisecond,
	})
	return r, store
}

// TestWaitReclaim_VerifiedWhenVRAMRises: the VRAM-delta gate passes when free rises past the bar.
func TestWaitReclaim_VerifiedWhenVRAMRises(t *testing.T) {
	// footprint 900, slack 500 → bar = 400. free rises 13000 → 13000 → 13500 (delta 500 >= 400).
	vram := &scriptedVRAM{seq: []int{13000, 13000, 13500}}
	r, _ := newRunnerForTest(t, vram)
	freeAfter, freed, verified, timedOut := r.waitReclaim(context.Background(), 13000, 900)
	if !verified || timedOut {
		t.Fatalf("expected verified, got verified=%v timedOut=%v (freed=%d)", verified, timedOut, freed)
	}
	if freeAfter != 13500 || freed != 500 {
		t.Fatalf("freeAfter=%d freed=%d, want 13500/500", freeAfter, freed)
	}
}

// TestWaitReclaim_TimesOutWhenVRAMStuck: reclaim never reaches the bar → timedOut, not verified.
func TestWaitReclaim_TimesOutWhenVRAMStuck(t *testing.T) {
	// free stays at 13000 (no reclaim). bar = 400. Should time out (2s config).
	vram := &scriptedVRAM{seq: []int{13000}}
	r, _ := newRunnerForTest(t, vram)
	start := time.Now()
	_, freed, verified, timedOut := r.waitReclaim(context.Background(), 13000, 900)
	if verified || !timedOut {
		t.Fatalf("expected timedOut, got verified=%v timedOut=%v", verified, timedOut)
	}
	if freed != 0 {
		t.Fatalf("freed=%d, want 0", freed)
	}
	if time.Since(start) < 1*time.Second {
		t.Errorf("should have waited ~timeout, took %s", time.Since(start))
	}
}

// TestRecordPeakVRAM_ComputesFootprint: peak = freeBeforeLoad - min free observed.
func TestRecordPeakVRAM_ComputesFootprint(t *testing.T) {
	// free before load 13000; during window drops to 12100 (footprint 900) then recovers.
	vram := &scriptedVRAM{seq: []int{12500, 12100, 12300}}
	r, store := newRunnerForTest(t, vram)
	ctx := context.Background()
	// Seed a HEALTHY instance so SetObservedPeak has a row.
	store.Create(ctx, instances.CreateParams{ID: "i1", ModelID: "m1", Engine: instances.EngineLlamaCPP, Reason: "t"})
	pid := 1
	store.Transition(ctx, "i1", instances.TransitionParams{To: instances.StateStarting, Reason: "s", PID: &pid})
	store.Transition(ctx, "i1", instances.TransitionParams{To: instances.StateHealthy, Reason: "h"})

	peak, err := r.RecordPeakVRAM(ctx, "i1", 13000)
	if err != nil {
		t.Fatalf("RecordPeakVRAM: %v", err)
	}
	if peak != 900 {
		t.Fatalf("peak = %d, want 900 (13000 - 12100)", peak)
	}
	got, _ := store.Get(ctx, "i1")
	if got.VRAMObsMiB != 900 {
		t.Fatalf("observed peak not persisted: %d", got.VRAMObsMiB)
	}
}

// TestBuildArgs_LocalhostAndFlags: host is always 127.0.0.1; compat flags + mmproj + no-webui.
func TestBuildArgs_LocalhostAndFlags(t *testing.T) {
	r, _ := newRunnerForTest(t, &scriptedVRAM{seq: []int{13000}})
	args := r.buildArgs(StartSpec{
		ModelPath:  `K:\m1.gguf`,
		MMProjPath: `K:\mmproj.gguf`,
		Ctx:        4096,
		Flags:      []string{"-ngl", "all", "-c", "4096", "-fa", "on"},
	}, 40100)

	joined := ""
	for _, a := range args {
		joined += a + " "
	}
	for _, want := range []string{"-m K:\\m1.gguf", "--host 127.0.0.1", "--port 40100", "--mmproj K:\\mmproj.gguf", "--no-webui"} {
		if !containsSub(joined, want) {
			t.Errorf("args missing %q; got: %s", want, joined)
		}
	}
	// -c should NOT be duplicated (compat flags already had it).
	if countSub(joined, "-c ") != 1 {
		t.Errorf("-c should appear once, args: %s", joined)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
func countSub(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
