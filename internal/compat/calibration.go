package compat

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/localhub/localhub/internal/compat/gguf"
)

// ObservedPeak is one recorded, MEASURED VRAM peak for a model — the driver-truth footprint
// captured during a real load (friction log #6: measured VRAM beats estimate). It maps 1:1 to
// a row in the observed_peaks table (migration 0003).
type ObservedPeak struct {
	ModelKey    string    // stable calibration key (models.id slug, or source-derived)
	PeakVRAMMiB int       // measured NVML VRAM delta peak (MiB); authoritative footprint
	Ctx         int       // context length the peak was measured at (tokens); 0 => unknown
	KVType      string    // KV cache dtype: f16 | q8_0 | q4_0 | tq3_0; "" => unknown
	BinaryHash  string    // engine binary id/hash that produced the peak; "" => not captured
	GPU         string    // GPU model the peak is valid for
	Driver      string    // driver version at measurement
	ObservedAt  time.Time // when the peak was measured
}

// CalibrationStore is the SQLite-backed calibration store. It reads and writes the
// observed_peaks table (migration 0003) and satisfies gguf.CalibrationStore (ObservedPeak),
// so the GGUF estimator prefers a measured peak over any computed estimate, returning it with
// Confidence "exact". The compat Engine consults it via EstimateWithCalibration.
//
// A measured peak is authoritative BECAUSE it is measured: it captures the fork's tq3_0 KV
// footprint, the mmproj VRAM, MoE active-vs-total behaviour, and the CUDA context overhead —
// all things the estimators are blind to (friction log #1,#4,#5,#6). It is keyed by a stable
// model_key (the models.id slug for inventory models), matching the key passed to the
// estimator via gguf.Source.WithModelKey.
type CalibrationStore struct {
	db *sql.DB
}

// NewCalibrationStore builds a CalibrationStore over the given *sql.DB (typically the daemon's
// *db.DB, which embeds *sql.DB). The observed_peaks table is created by migration 0003; this
// constructor does not create it.
func NewCalibrationStore(db *sql.DB) *CalibrationStore {
	return &CalibrationStore{db: db}
}

// Compile-time assertion that *CalibrationStore satisfies the GGUF estimator's interface, so
// it can be injected as the calibration store the estimator consults first.
var _ gguf.CalibrationStore = (*CalibrationStore)(nil)

// ObservedPeak returns the recorded peak VRAM in MiB for modelKey, and ok=false if none is
// recorded. It satisfies gguf.CalibrationStore. A DB error is treated as "no calibration"
// (ok=false): a broken calibration read must NEVER block a verdict — the estimator simply
// falls through to parse/fallback (fail-safe, per the "always a verdict" spec §4). The read
// is best-effort and bounded by a short context so a locked DB cannot stall the estimator.
func (c *CalibrationStore) ObservedPeak(modelKey string) (mib int, ok bool) {
	if c == nil || c.db == nil || modelKey == "" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var v int
	err := c.db.QueryRowContext(ctx,
		`SELECT peak_vram_mib FROM observed_peaks WHERE model_key = ?`, modelKey).Scan(&v)
	if err != nil {
		// sql.ErrNoRows (no calibration yet) and any transient DB error both mean "no peak".
		return 0, false
	}
	return v, true
}

// Lookup returns the full ObservedPeak row for modelKey (for the verdict note "observed peak
// from <date> at ctx N" and provenance display), and ok=false if none is recorded.
func (c *CalibrationStore) Lookup(ctx context.Context, modelKey string) (ObservedPeak, bool, error) {
	if c == nil || c.db == nil || modelKey == "" {
		return ObservedPeak{}, false, nil
	}
	const q = `SELECT model_key, peak_vram_mib, COALESCE(ctx,0), COALESCE(kv_type,''),
	       COALESCE(binary_hash,''), COALESCE(gpu,''), COALESCE(driver,''), observed_at
	FROM observed_peaks WHERE model_key = ?`
	var (
		p          ObservedPeak
		observedAt string
	)
	err := c.db.QueryRowContext(ctx, q, modelKey).Scan(
		&p.ModelKey, &p.PeakVRAMMiB, &p.Ctx, &p.KVType, &p.BinaryHash, &p.GPU, &p.Driver, &observedAt)
	if err == sql.ErrNoRows {
		return ObservedPeak{}, false, nil
	}
	if err != nil {
		return ObservedPeak{}, false, fmt.Errorf("read observed_peak %q: %w", modelKey, err)
	}
	p.ObservedAt = parseSQLiteTime(observedAt)
	return p, true, nil
}

// Record UPSERTs a measured peak. A later, better measurement for the same model_key REPLACES
// the previous row (one authoritative peak per key; the newest measurement wins). model_key
// and a positive peak are required. observed_at defaults to now when the caller leaves it zero.
func (c *CalibrationStore) Record(ctx context.Context, p ObservedPeak) error {
	if c == nil || c.db == nil {
		return fmt.Errorf("calibration store not configured")
	}
	if p.ModelKey == "" {
		return fmt.Errorf("record observed peak: model_key is required")
	}
	if p.PeakVRAMMiB <= 0 {
		return fmt.Errorf("record observed peak for %q: peak_vram_mib must be > 0, got %d", p.ModelKey, p.PeakVRAMMiB)
	}
	observedAt := p.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	const q = `INSERT INTO observed_peaks
	    (model_key, peak_vram_mib, ctx, kv_type, binary_hash, gpu, driver, observed_at)
	    VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(model_key) DO UPDATE SET
	    peak_vram_mib = excluded.peak_vram_mib,
	    ctx           = excluded.ctx,
	    kv_type       = excluded.kv_type,
	    binary_hash   = excluded.binary_hash,
	    gpu           = excluded.gpu,
	    driver        = excluded.driver,
	    observed_at   = excluded.observed_at`
	_, err := c.db.ExecContext(ctx, q,
		p.ModelKey, p.PeakVRAMMiB,
		nullIntIfZero(p.Ctx), nullStrIfEmpty(p.KVType), nullStrIfEmpty(p.BinaryHash),
		nullStrIfEmpty(p.GPU), nullStrIfEmpty(p.Driver),
		observedAt.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return fmt.Errorf("record observed peak %q: %w", p.ModelKey, err)
	}
	return nil
}

// parseSQLiteTime parses the datetime('now') text format ("2006-01-02 15:04:05", UTC) SQLite
// writes, falling back to RFC3339. A zero time is returned on an unparseable value.
func parseSQLiteTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func nullStrIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIntIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
