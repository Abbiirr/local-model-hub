package instances

import "testing"

// TestCanTransition_LegalEdges asserts every legal edge in the state machine (spec-digest §6).
func TestCanTransition_LegalEdges(t *testing.T) {
	legal := []struct{ from, to State }{
		{StateRequested, StateStarting},
		{StateRequested, StateFailed},
		{StateStarting, StateHealthy},
		{StateStarting, StateFailed},
		{StateStarting, StateStopped},
		{StateHealthy, StateDraining},
		{StateHealthy, StateStopped},
		{StateHealthy, StateFailed},
		{StateDraining, StateStopped},
		{StateDraining, StateFailed},
	}
	for _, tc := range legal {
		if !CanTransition(tc.from, tc.to) {
			t.Errorf("expected %s → %s to be LEGAL", tc.from, tc.to)
		}
	}
}

// TestCanTransition_IllegalEdges asserts representative illegal edges are rejected, including
// backward moves, terminal-state exits, and skips.
func TestCanTransition_IllegalEdges(t *testing.T) {
	illegal := []struct{ from, to State }{
		{StateRequested, StateHealthy},  // skip STARTING
		{StateRequested, StateDraining}, // skip
		{StateRequested, StateStopped},  // skip STARTING
		{StateStarting, StateDraining},  // must be HEALTHY first
		{StateStarting, StateRequested}, // backward
		{StateHealthy, StateRequested},  // backward
		{StateHealthy, StateStarting},   // backward
		{StateDraining, StateHealthy},   // backward
		{StateStopped, StateStarting},   // terminal exit
		{StateStopped, StateHealthy},    // terminal exit
		{StateFailed, StateStarting},    // terminal exit
		{StateFailed, StateStopped},     // terminal exit
		{StateStopped, StateStopped},    // self on terminal
	}
	for _, tc := range illegal {
		if CanTransition(tc.from, tc.to) {
			t.Errorf("expected %s → %s to be ILLEGAL", tc.from, tc.to)
		}
	}
}

// TestState_Valid rejects unknown states and accepts the six known ones.
func TestState_Valid(t *testing.T) {
	for _, s := range []State{StateRequested, StateStarting, StateHealthy, StateDraining, StateStopped, StateFailed} {
		if !s.Valid() {
			t.Errorf("%s should be Valid", s)
		}
	}
	for _, s := range []State{"", "RUNNING", "healthy ", "unknown"} {
		if s.Valid() {
			t.Errorf("%q should be invalid", s)
		}
	}
}

// TestNormalizeState upper-cases + trims a stored value.
func TestNormalizeState(t *testing.T) {
	if got := normalizeState(" healthy "); got != StateHealthy {
		t.Errorf("normalizeState(' healthy ') = %q, want HEALTHY", got)
	}
	if got := normalizeState("Draining"); got != StateDraining {
		t.Errorf("normalizeState('Draining') = %q, want DRAINING", got)
	}
}

// TestTerminal identifies the two end states.
func TestTerminal(t *testing.T) {
	if !StateStopped.Terminal() || !StateFailed.Terminal() {
		t.Error("STOPPED and FAILED must be terminal")
	}
	for _, s := range []State{StateRequested, StateStarting, StateHealthy, StateDraining} {
		if s.Terminal() {
			t.Errorf("%s must not be terminal", s)
		}
	}
}
