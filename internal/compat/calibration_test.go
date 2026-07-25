package compat

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/localhub/localhub/internal/compat/gguf"
	"github.com/localhub/localhub/internal/db"
)

// openCalibDB opens a migrated temp DB and returns a CalibrationStore over it.
func openCalibDB(t *testing.T) (*db.DB, *CalibrationStore) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "calib.db")
	d, err := db.Open(p)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d, NewCalibrationStore(d.DB)
}

// --- CRUD -------------------------------------------------------------------------------

func TestCalibrationStore_RecordThenObservedPeak(t *testing.T) {
	_, store := openCalibDB(t)
	ctx := context.Background()

	// No peak yet.
	if _, ok := store.ObservedPeak("glm-ocr-q8_0", 4096, gguf.KVCacheF16); ok {
		t.Fatal("ObservedPeak on empty store must be ok=false")
	}

	err := store.Record(ctx, ObservedPeak{
		ModelKey: "glm-ocr-q8_0", PeakVRAMMiB: 1105, Ctx: 4096, KVType: "f16",
		GPU: "NVIDIA GeForce RTX 4060 Ti", Driver: "576.40",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	mib, ok := store.ObservedPeak("glm-ocr-q8_0", 4096, gguf.KVCacheF16)
	if !ok || mib != 1105 {
		t.Fatalf("ObservedPeak = (%d,%v), want (1105,true)", mib, ok)
	}

	// Full row via Lookup.
	p, ok, err := store.Lookup(ctx, "glm-ocr-q8_0")
	if err != nil || !ok {
		t.Fatalf("Lookup = (_,%v,%v), want (_,true,nil)", ok, err)
	}
	if p.PeakVRAMMiB != 1105 || p.Ctx != 4096 || p.KVType != "f16" || p.Driver != "576.40" {
		t.Fatalf("Lookup row = %+v", p)
	}
	if p.ObservedAt.IsZero() {
		t.Fatal("Lookup ObservedAt must be stamped")
	}
}

func TestCalibrationStore_RecordUpsertsNewestWins(t *testing.T) {
	_, store := openCalibDB(t)
	ctx := context.Background()

	if err := store.Record(ctx, ObservedPeak{ModelKey: "m", PeakVRAMMiB: 1000, Ctx: 4096}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	// A later, better measurement replaces the previous row.
	if err := store.Record(ctx, ObservedPeak{ModelKey: "m", PeakVRAMMiB: 1234, Ctx: 8192, KVType: "q4_0"}); err != nil {
		t.Fatalf("second Record: %v", err)
	}
	mib, ok := store.ObservedPeak("m", 8192, gguf.KVCacheQ4_0)
	if !ok || mib != 1234 {
		t.Fatalf("after upsert ObservedPeak = (%d,%v), want (1234,true)", mib, ok)
	}
	p, _, _ := store.Lookup(ctx, "m")
	if p.Ctx != 8192 || p.KVType != "q4_0" {
		t.Fatalf("upsert did not replace provenance: %+v", p)
	}
}

func TestCalibrationStore_RecordRejectsInvalid(t *testing.T) {
	_, store := openCalibDB(t)
	ctx := context.Background()
	if err := store.Record(ctx, ObservedPeak{ModelKey: "", PeakVRAMMiB: 500}); err == nil {
		t.Error("Record with empty model_key must error")
	}
	if err := store.Record(ctx, ObservedPeak{ModelKey: "m", PeakVRAMMiB: 0}); err == nil {
		t.Error("Record with non-positive peak must error")
	}
	if err := store.Record(ctx, ObservedPeak{ModelKey: "m", PeakVRAMMiB: -1}); err == nil {
		t.Error("Record with negative peak must error")
	}
}

func TestCalibrationStore_ObservedPeakFailSafeOnNilOrMissing(t *testing.T) {
	// A nil store / nil db must never panic and must report "no calibration".
	var nilStore *CalibrationStore
	if _, ok := nilStore.ObservedPeak("x", 4096, gguf.KVCacheF16); ok {
		t.Error("nil store must report ok=false")
	}
	_, store := openCalibDB(t)
	if _, ok := store.ObservedPeak("", 4096, gguf.KVCacheF16); ok {
		t.Error("empty key must report ok=false")
	}
}

func TestCalibrationStore_SatisfiesGGUFInterface(t *testing.T) {
	_, store := openCalibDB(t)
	// Compile-time assertion also exists in calibration.go; this exercises it at runtime.
	var iface gguf.CalibrationStore = store
	if iface == nil {
		t.Fatal("CalibrationStore must satisfy gguf.CalibrationStore")
	}
}

// --- Integration: verdict uses the observed peak ----------------------------------------

// TestVerdictUsesObservedPeak proves the end-to-end measured-beats-estimate loop at the compat
// engine boundary: an engine built via NewWithCalibration(store) consults the store, so once a
// peak is recorded the verdict's estimated VRAM equals the MEASURED peak (not the parse/fallback
// number) and Confidence is "exact".
func TestVerdictUsesObservedPeak(t *testing.T) {
	_, store := openCalibDB(t)
	ctx := context.Background()

	const key = "qwen3.6-27b-tq3_4s"
	// A TQ3 model gguf-parser cannot parse: absent calibration, the fallback estimate would be
	// a large file-size heuristic (~14900 MiB). We instead record a measured peak that fits.
	if err := store.Record(ctx, ObservedPeak{
		ModelKey: key, PeakVRAMMiB: 10800, Ctx: 8192, KVType: "f16",
		GPU: "NVIDIA GeForce RTX 4060 Ti", Driver: "576.40",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Real engine wired to the real store (NOT a fake estimator) — this is the wiring under test.
	e := NewWithCalibration(store)

	desc := ModelDescriptor{
		ID:           key,
		DisplayName:  "Qwen3.6-27B-TQ3_4S",
		Source:       "local_path",
		Format:       FormatGGUF,
		Quantization: "tq3_4s",
		LocalPath:    "/does/not/exist.gguf", // never read: calibration short-circuits parse
		GGUFSource:   gguf.LocalFile("/does/not/exist.gguf").WithModelKey(key),
	}

	// Baseline free VRAM 13289 MiB (the reference box). usable = 13289*0.92 ≈ 12226 MiB.
	// Measured peak 10800 < usable → runs, with the measured number.
	v := e.Evaluate(ctx, Request{Descriptor: desc, RequestedCtx: 8192},
		snap(13289, 16380, "8.9"),
		availWith(cudaFork))

	if v.Status != StatusRuns {
		t.Fatalf("status = %q, want runs; reason=%q", v.Status, v.Reason)
	}
	if v.EstimatedVRAMMiB != 10800 {
		t.Fatalf("EstimatedVRAMMiB = %d, want 10800 (the measured peak, not a heuristic)", v.EstimatedVRAMMiB)
	}
	if v.Confidence != ConfidenceExact {
		t.Fatalf("Confidence = %q, want exact (measured peak)", v.Confidence)
	}
	if !strings.Contains(strings.ToLower(v.Reason), "measured") {
		t.Fatalf("reason must state the number is measured; got %q", v.Reason)
	}
	if v.Estimator != "calibration/observed-peak" {
		t.Fatalf("Estimator = %q, want calibration/observed-peak", v.Estimator)
	}
}

// TestVerdictTightensAfterRecording proves the BEFORE/AFTER "tightening" story P2.5 evidences,
// using a fake estimator for the BEFORE (deterministic fallback) so the flip is visible in one
// test: the engine consults the real store first; recording a peak overrides the fake estimate.
func TestVerdictTightensAfterRecording(t *testing.T) {
	_, store := openCalibDB(t)
	ctx := context.Background()
	const key = "cal-tighten"

	// The engine consults the real store first (via NewWithCalibration's seam), then a fake
	// fallback estimate. We simulate the fallback by intercepting AFTER the store misses.
	e := NewWithCalibration(store)
	fallback := gguf.Result{VRAMMiB: 9000, Confidence: gguf.ConfidenceFallback, ParserVersion: gguf.ParserVersion + "+fallback-heuristic"}
	e.ggufEstimate = func(_ context.Context, _ gguf.Source, opts gguf.Options) (gguf.Result, error) {
		if mib, ok := store.ObservedPeak(key, opts.ContextLen, opts.KVCacheType); ok {
			return gguf.Result{VRAMMiB: mib, Confidence: gguf.ConfidenceExact, ParserVersion: "calibration/observed-peak"}, nil
		}
		return fallback, nil
	}

	desc := ModelDescriptor{
		ID: key, DisplayName: key, Source: "local_path", Format: FormatGGUF, Quantization: "tq3_4s",
		GGUFSource: gguf.LocalFile("/x.gguf").WithModelKey(key),
	}
	sn := snap(13289, 16380, "8.9")

	// BEFORE: fallback estimate 9000 MiB → runs (fits) but Confidence fallback.
	before := e.Evaluate(ctx, Request{Descriptor: desc, RequestedCtx: 8192}, sn, availWith(cudaFork))
	if before.Confidence != ConfidenceFallback || before.EstimatedVRAMMiB != 9000 {
		t.Fatalf("BEFORE: got vram=%d conf=%q, want 9000/fallback", before.EstimatedVRAMMiB, before.Confidence)
	}

	// Record a measured peak that is smaller and authoritative.
	if err := store.Record(ctx, ObservedPeak{ModelKey: key, PeakVRAMMiB: 7000, Ctx: 8192, KVType: "f16"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// AFTER: same request now returns the MEASURED number with Confidence exact — tightened.
	after := e.Evaluate(ctx, Request{Descriptor: desc, RequestedCtx: 8192}, sn, availWith(cudaFork))
	if after.Confidence != ConfidenceExact || after.EstimatedVRAMMiB != 7000 {
		t.Fatalf("AFTER: got vram=%d conf=%q, want 7000/exact", after.EstimatedVRAMMiB, after.Confidence)
	}
	if before.EstimatedVRAMMiB <= after.EstimatedVRAMMiB && before.Confidence == after.Confidence {
		t.Fatal("verdict did not tighten from BEFORE to AFTER")
	}
}
