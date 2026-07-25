// Package instances owns the LocalHub instance lifecycle: the state machine
// (REQUESTED → STARTING → HEALTHY → DRAINING → STOPPED | FAILED), its persistence to the
// `instances` table (with an append-only transition audit), the port pool for spawned
// engines, and daemon-startup reconciliation of orphaned/dead child processes.
//
// This package is engine-agnostic. It knows HOW to move an instance between states, persist
// the move, allocate/free a port, and reconcile a recorded process against reality — but it
// does NOT know how to spawn llama-server (that is internal/engines/llamacpp, which drives
// this package). Keeping the state machine here means P4.3 (Ollama) and P4.4 (vLLM) reuse the
// same lifecycle and the residency manager (P5) has one place to read/observe instance state.
//
// Design rules (spec-digest §6, CLAUDE.md correctness bars):
//   - Legal-transition enforcement: an illegal transition is a PROGRAMMING error, returned as
//     a typed *IllegalTransitionError and logged — never silently applied.
//   - Every transition is persisted with a timestamp + a MANDATORY human-readable reason.
//   - DRAINING is "marked for eviction but has in-flight requests" — the residency layer must
//     not kill a DRAINING instance until it drains (never-kill-active-streams). This package
//     enforces the transition legality; the kill decision lives in residency.
//   - launch_flags_json is written at spawn time so the reproduce export (Phase 3) can name
//     engine + flags for a real run.
package instances

import (
	"fmt"
	"strings"
)

// State is the instance lifecycle state. The six values are the exact vocabulary from the
// instances DDL (data-model.md) and spec-digest §6 — do not add to it.
type State string

const (
	// StateRequested: the instance row exists; nothing has been spawned yet.
	StateRequested State = "REQUESTED"
	// StateStarting: the process has been spawned; we are polling /health.
	StateStarting State = "STARTING"
	// StateHealthy: the health endpoint answered; the instance is serving.
	StateHealthy State = "HEALTHY"
	// StateDraining: marked for stop/eviction but may have in-flight requests — do NOT kill
	// until drained (never-kill-active-streams, residency-scheduler.md).
	StateDraining State = "DRAINING"
	// StateStopped: the process exited (graceful stop or reaped-dead) and VRAM was reclaimed.
	StateStopped State = "STOPPED"
	// StateFailed: the instance never became healthy, or was reaped as an orphan/mismatch.
	StateFailed State = "FAILED"
)

// terminal reports whether s is an end state (no further transitions allowed).
func (s State) terminal() bool { return s == StateStopped || s == StateFailed }

// Terminal is the exported form of terminal (used by adapters to short-circuit a stop on an
// already-ended instance).
func (s State) Terminal() bool { return s.terminal() }

// Valid reports whether s is one of the six known states.
func (s State) Valid() bool {
	switch s {
	case StateRequested, StateStarting, StateHealthy, StateDraining, StateStopped, StateFailed:
		return true
	default:
		return false
	}
}

// legalTransitions is the adjacency list of the state machine. A transition (from → to) is
// legal iff `to` appears in legalTransitions[from]. This is the single source of truth for
// legality; CanTransition and the persisting Store consult it.
//
// The graph (spec-digest §6, architecture.md instance state machine):
//
//	REQUESTED → STARTING | FAILED            (spawn, or spawn setup failed)
//	STARTING  → HEALTHY  | FAILED | STOPPED  (health ok; health timeout/crash; stopped mid-start)
//	HEALTHY   → DRAINING | STOPPED | FAILED  (evict-with-drain; direct stop; crashed while serving)
//	DRAINING  → STOPPED  | FAILED            (drained then stopped; drain aborted → failed)
//	STOPPED   → (none)                       terminal
//	FAILED    → (none)                       terminal
//
// STARTING→STOPPED is legal because a stop requested during startup (or a reconcile that finds
// the process already gone) lands the row in STOPPED without a health success.
var legalTransitions = map[State][]State{
	StateRequested: {StateStarting, StateFailed},
	StateStarting:  {StateHealthy, StateFailed, StateStopped},
	StateHealthy:   {StateDraining, StateStopped, StateFailed},
	StateDraining:  {StateStopped, StateFailed},
	StateStopped:   {},
	StateFailed:    {},
}

// IllegalTransitionError is returned when a caller attempts a transition not in
// legalTransitions. It is a typed error (spec-digest §6: "illegal transition = programming
// error, returned as typed error + logged") so callers can assert on it and the daemon can log
// it distinctly from ordinary failures.
type IllegalTransitionError struct {
	InstanceID string
	From       State
	To         State
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("illegal instance transition %s → %s (instance %s): not a legal edge in the state machine",
		e.From, e.To, e.InstanceID)
}

// CanTransition reports whether from → to is a legal edge. An unknown `from` (not in the map)
// or a terminal `from` yields false. It is pure (no I/O) so it is trivially unit-testable and
// callable as a guard before attempting a persisted transition.
func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	for _, allowed := range legalTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// LegalNextStates returns the states reachable in one step from `from` (empty for terminal or
// unknown states). Exposed for diagnostics and tests.
func LegalNextStates(from State) []State {
	out := make([]State, len(legalTransitions[from]))
	copy(out, legalTransitions[from])
	return out
}

// normalizeState upper-cases and trims a state string read back from the DB so a stored
// "healthy" or " HEALTHY " still compares equal to StateHealthy. Unknown values are returned
// as-is (and will fail State.Valid()).
func normalizeState(s string) State {
	return State(strings.ToUpper(strings.TrimSpace(s)))
}
