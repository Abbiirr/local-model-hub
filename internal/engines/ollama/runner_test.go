package ollama

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/localhub/localhub/internal/instances"
)

// scriptedVRAM returns a rising free-VRAM sequence: it starts low (model resident) and jumps up
// after the model is unloaded, so the Runner's stop-verify delta clears the bar.
type scriptedVRAM struct {
	mu    sync.Mutex
	value int
	after int // once callCount >= after, return the high value
	high  int
	calls int
}

func (s *scriptedVRAM) FreeVRAMMiB(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls >= s.after {
		return s.high, nil
	}
	return s.value, nil
}

func TestRunner_WarmToHealthy(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()
	store := mirrorTestStore(t)
	r := NewRunner(New(srv.URL, ""), store, nil, RunnerConfig{
		WarmHealthTimeout: 3 * time.Second, WarmPollInterval: 50 * time.Millisecond,
	})

	inst, err := r.Warm(context.Background(), WarmSpec{
		InstanceID: "w1", ModelID: "ollama-mistral-latest", OllamaTag: "mistral:latest",
	})
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if inst.State != instances.StateHealthy {
		t.Errorf("state: got %q want HEALTHY", inst.State)
	}
	if inst.VRAMObsMiB <= 0 {
		t.Error("expected observed VRAM recorded from size_vram")
	}
}

func TestRunner_UnloadVerifiesVRAMDelta(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()
	store := mirrorTestStore(t)
	// Free VRAM: 8000 while resident, jumps to 9000 after a couple probe reads (post-unload).
	vram := &scriptedVRAM{value: 8000, after: 3, high: 9000}
	r := NewRunner(New(srv.URL, ""), store, vram, RunnerConfig{
		WarmHealthTimeout: 3 * time.Second, WarmPollInterval: 50 * time.Millisecond,
		Unload:                UnloadConfig{PollTimeout: 3 * time.Second, PollInterval: 50 * time.Millisecond},
		StopVerifyTimeout:     3 * time.Second,
		StopVRAMDeltaSlackMiB: 500,
	})
	ctx := context.Background()

	if _, err := r.Warm(ctx, WarmSpec{InstanceID: "w1", ModelID: "ollama-mistral-latest", OllamaTag: "mistral:latest"}); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	// The fake daemon reports a 700 MiB footprint; a 1000 MiB reclaim (9000-8000) clears 700-500.
	res, err := r.Stop(ctx, "w1", "mistral:latest")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.Gone {
		t.Errorf("expected model gone from /api/ps, reason=%q", res.Reason)
	}
	if !res.Verified {
		t.Errorf("expected VRAM-delta VERIFIED, got reason=%q (freed=%d)", res.Reason, res.FreedMiB)
	}
	inst, _ := store.Get(ctx, "w1")
	if inst.State != instances.StateStopped {
		t.Errorf("state: got %q want STOPPED", inst.State)
	}
}

func TestRunner_UnloadNoProbe_GoneIsVerified(t *testing.T) {
	f := newFakeDaemon()
	srv := f.server(t)
	defer srv.Close()
	store := mirrorTestStore(t)
	r := NewRunner(New(srv.URL, ""), store, nil, RunnerConfig{ // nil probe
		WarmHealthTimeout: 3 * time.Second, WarmPollInterval: 50 * time.Millisecond,
		Unload: UnloadConfig{PollTimeout: 3 * time.Second, PollInterval: 50 * time.Millisecond},
	})
	ctx := context.Background()
	if _, err := r.Warm(ctx, WarmSpec{InstanceID: "w1", ModelID: "ollama-mistral-latest", OllamaTag: "mistral:latest"}); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	res, err := r.Stop(ctx, "w1", "mistral:latest")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.Gone || !res.Verified {
		t.Errorf("with no probe, Gone from /api/ps is the verify signal; got gone=%v verified=%v", res.Gone, res.Verified)
	}
}
