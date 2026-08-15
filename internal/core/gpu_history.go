package core

import (
	"sync"
	"time"

	"github.com/localhub/localhub/internal/hardware"
)

// This file implements the GPU telemetry ring buffer (PLAN-ui-gpu-rework.md §2.4).
//
// Deliberately IN-MEMORY and lossy. This is disposable telemetry for drawing sparklines — a few
// minutes of "is VRAM climbing, is it thermal throttling". Persisting it would add write
// amplification to a SQLite file that is on the critical path for instance state, in exchange
// for history nobody asks for after a restart.

// GPUSample is one point of GPU telemetry.
type GPUSample struct {
	At time.Time `json:"t"`
	// VRAMUsedMiB is used (not free) so a rising line means rising pressure, which is the
	// direction a human expects a "memory" graph to move.
	VRAMUsedMiB int     `json:"vram_used_mib"`
	VRAMFreeMiB int     `json:"vram_free_mib"`
	UtilPct     float64 `json:"util"`
	TempC       float64 `json:"temp"`
	PowerW      float64 `json:"power"`
}

// gpuHistoryCapacity is how many samples are retained. At the default poll cadence this is
// several minutes, enough to see a model load, a generation burst, and the decay after — the
// three shapes worth looking at. ~60 bytes/sample, so the whole buffer is trivial.
const gpuHistoryCapacity = 240

// gpuHistory is a fixed-size circular buffer of GPU samples for GPU 0.
//
// Only GPU 0 is tracked, matching the compat engine and admission (compat/engine.go reads
// GPUs[0]). Tracking devices the fit logic ignores would imply a multi-GPU story the rest of
// the system does not have (ADR-007, single box).
type gpuHistory struct {
	mu   sync.Mutex
	buf  []GPUSample
	next int
	full bool
}

func newGPUHistory() *gpuHistory {
	return &gpuHistory{buf: make([]GPUSample, gpuHistoryCapacity)}
}

// add appends a sample, overwriting the oldest once full.
func (h *gpuHistory) add(s GPUSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf[h.next] = s
	h.next = (h.next + 1) % len(h.buf)
	if h.next == 0 {
		h.full = true
	}
}

// snapshot returns the samples in chronological order (oldest first). It returns a COPY: the
// caller serializes this to JSON on an HTTP goroutine while the sampler keeps writing, so
// handing out the backing array would be a data race.
func (h *gpuHistory) snapshot() []GPUSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.full {
		out := make([]GPUSample, h.next)
		copy(out, h.buf[:h.next])
		return out
	}
	out := make([]GPUSample, 0, len(h.buf))
	out = append(out, h.buf[h.next:]...)
	out = append(out, h.buf[:h.next]...)
	return out
}

// RecordGPUSample stores one telemetry point from a hardware snapshot. Snapshots with no GPU
// are ignored rather than recorded as zeros — a flat line at 0 would read as "idle GPU" when
// the truth is "no GPU".
//
// Safe to call concurrently, and safe to call before any reader exists.
func (s *Service) RecordGPUSample(snap hardware.Snapshot) {
	if s.gpuHist == nil || len(snap.GPUs) == 0 {
		return
	}
	g := snap.GPUs[0]
	at := snap.CapturedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	s.gpuHist.add(GPUSample{
		At:          at,
		VRAMUsedMiB: g.VRAMUsedMiB,
		VRAMFreeMiB: g.VRAMFreeMiB,
		UtilPct:     g.UtilizationPct,
		TempC:       g.TemperatureC,
		PowerW:      g.PowerDrawW,
	})
}

// GPUHistory returns retained GPU samples, oldest first. Empty (never nil-panicking) before the
// first sample lands, so a dashboard loaded immediately after start renders an empty chart
// rather than erroring.
func (s *Service) GPUHistory() []GPUSample {
	if s.gpuHist == nil {
		return nil
	}
	return s.gpuHist.snapshot()
}
