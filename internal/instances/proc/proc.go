// Package proc is the OS process-management abstraction for spawned engine children
// (llama.cpp, vLLM). It is the reusable scaffolding P4.2 (llama.cpp) and P4.4 (vLLM) share:
// spawn a child bound to a Job Object so the whole tree can be killed reliably, poll whether a
// PID is alive, graceful-then-hard stop with the lmwrapper 5s rule, and kill-tree an orphan we
// only have a PID for (reconcile path).
//
// PLAN.md conflict-resolution 5 (Windows lessons, research/lmwrapper-reuse-notes.md §7):
//   - CREATE_NO_WINDOW so a spawned console app does not flash/steal a window.
//   - Job Objects: assign the child to a job at spawn (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) so
//     closing the job handle kills the entire tree — more reliable than taskkill /T for
//     re-parented grandchildren. Chosen as PRIMARY kill; taskkill /T /F is the reconcile-time
//     fallback (we hold no job handle for a process from a prior daemon).
//   - Graceful first: terminate() then kill() after 5s (lmwrapper worker lifecycle). On Windows
//     "terminate" (no /F) asks the process to exit; after the wait budget we force-kill via the
//     job (or taskkill /F for an orphan).
//   - Memory-free polling is done by the caller (the adapter) via the hardware probe, NOT here —
//     this package only manages the process; the VRAM-delta gate lives in the adapter.
//
// The Windows implementation is in proc_windows.go (x/sys/windows Job Objects). A portable
// fallback (proc_other.go) uses os.Process + SIGTERM/SIGKILL so the package builds and unit-
// tests on non-Windows CI. Both satisfy the same Process interface.
package proc

import (
	"context"
	"io"
	"time"
)

// GraceTimeout is the lmwrapper "terminate then kill after 5s" budget: after a graceful stop
// request we wait this long for the process to exit before force-killing (research/lmwrapper-
// reuse-notes.md §7). Exposed so callers/tests can reference the same value.
const GraceTimeout = 5 * time.Second

// SpawnSpec describes a child process to launch.
type SpawnSpec struct {
	// Path is the absolute executable path.
	Path string
	// Args are the arguments (NOT including argv[0]).
	Args []string
	// Env is the full environment for the child (typically os.Environ() with ExtraPath
	// prepended to PATH for the CUDA fork's DLLs). Empty => inherit the parent env.
	Env []string
	// Dir is the working directory (empty => inherit).
	Dir string
	// Stdout/Stderr receive the child's output (the adapter tees these to a ring buffer + log
	// file). nil => discard.
	Stdout io.Writer
	Stderr io.Writer
}

// Process is a spawned child under a Job Object (or the portable fallback). It exposes the PID,
// a graceful/forceful stop, and a wait. Safe for concurrent Stop/Wait calls.
type Process interface {
	// PID returns the OS process id.
	PID() int
	// Wait blocks until the process exits (or ctx is done) and returns the exit error, if any.
	Wait(ctx context.Context) error
	// Stop performs a graceful stop (ask to exit), waits up to GraceTimeout, then force-kills
	// the whole tree via the Job Object. Returns nil once the process is confirmed gone.
	Stop(ctx context.Context) error
	// Kill force-kills the whole tree immediately (no grace period).
	Kill() error
	// Done reports whether the process has already exited.
	Done() bool
}

// Spawn launches spec as a Job-Object-managed child (Windows) or a process-group child
// (fallback). The returned Process is already running.
func Spawn(spec SpawnSpec) (Process, error) { return spawn(spec) }

// IsAlive reports whether a process with the given PID is currently running. Used at reconcile
// time where we have only a recorded PID (no handle) from a prior daemon. It is best-effort:
// on any ambiguity it errs toward "not alive" for safety of the reconcile decision (a dead
// process wrongly reported alive would leave a zombie row; a live process wrongly reported dead
// is caught by the subsequent health check before we act).
func IsAlive(pid int) bool { return isAlive(pid) }

// KillTree force-kills the process tree rooted at pid (reconcile-time orphan reaping, where we
// hold no Job Object handle). On Windows this is `taskkill /PID <pid> /T /F`; on the fallback
// it signals the process group. Best-effort: returns an error only if the kill command itself
// could not be issued.
func KillTree(pid int) error { return killTree(pid) }
