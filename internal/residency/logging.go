package residency

// logging.go is Phase 5 fix 2 (admission observability). The residency package stays pure — it
// takes a tiny structured-logger INTERFACE, not a concrete *slog.Logger — so tests can capture
// log lines without a real logging backend and the package does not hard-depend on log/slog.
// main wires a SlogAdmissionLogger over the daemon's slog.Logger.
//
// The manager emits exactly:
//   - one AdmissionDecision line per Admit call (the decision + all the numbers behind it), and
//   - one EvictionReRead line per eviction (the victim + expected freed + the free-VRAM RE-READ
//     value, proving the anti-OOM per-eviction probe re-read happened).
//
// These are the structured lines the storm evidence quotes verbatim.

// AdmissionLogger is the structured-logging seam admission uses. A nil logger is a no-op (the
// manager guards), so wiring it is optional and tests can pass a capturing fake.
type AdmissionLogger interface {
	// AdmissionDecision logs one admission decision with its numbers.
	AdmissionDecision(d AdmissionLog)
	// EvictionReRead logs one eviction and the free-VRAM value RE-READ from the probe after it.
	EvictionReRead(e EvictionLog)
}

// AdmissionLog is the structured payload of one admission decision (fix 2 spec: {model, decision,
// need_mib, free_mib, margin, evictions, reason}).
type AdmissionLog struct {
	Model         string
	Engine        string
	Decision      string
	Class         string
	FootprintMiB  int
	NeedMiB       int
	MarginMiB     int
	FreeBeforeMiB int
	FreeAfterMiB  int
	// Evictions is the per-victim detail (id, expected freed, free-VRAM re-read after the stop).
	Evictions []EvictionLog
	Reason    string
}

// EvictionLog is the structured payload of one eviction re-read (fix 2 spec: {victim,
// freed_expected, free_after_reread}).
type EvictionLog struct {
	Victim             string
	FreedExpectedMiB   int
	FreedMeasuredMiB   int
	FreeAfterReReadMiB int
}

// logDecision emits the AdmissionDecision line (nil-safe).
func (m *Manager) logDecision(spec AdmitSpec, res AdmitResult, evictions []EvictionLog) {
	if m.logger == nil {
		return
	}
	m.logger.AdmissionDecision(AdmissionLog{
		Model:         spec.ModelID,
		Engine:        spec.Engine,
		Decision:      string(res.Decision),
		Class:         spec.Class.String(),
		FootprintMiB:  res.FootprintMiB,
		NeedMiB:       res.NeedMiB,
		MarginMiB:     m.safetyMarginMiB,
		FreeBeforeMiB: res.FreeBeforeMiB,
		FreeAfterMiB:  res.FreeAfterMiB,
		Evictions:     evictions,
		Reason:        res.Reason,
	})
}
