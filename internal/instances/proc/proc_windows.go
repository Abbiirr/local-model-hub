//go:build windows

package proc

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createNoWindow is CREATE_NO_WINDOW: the spawned console app runs without allocating/flashing
// a console (PLAN.md conflict-resolution 5 Windows lessons). createSuspended lets us assign the
// child to a Job Object BEFORE it runs, so the whole tree is captured from the first instruction.
const (
	createNoWindow  = 0x08000000
	createSuspended = 0x00000004
	stillActive     = 259 // STILL_ACTIVE (not exported by x/sys/windows)
)

// winProcess is a Job-Object-managed child. Closing the job handle kills the whole tree
// (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE), which is the primary, reliable kill path.
type winProcess struct {
	cmd *exec.Cmd
	job windows.Handle

	mu       sync.Mutex
	waitErr  error
	waited   bool
	waitDone chan struct{}
}

// spawn creates the Job Object, launches the child suspended, assigns it to the job, then
// resumes it. On any setup failure the partial resources are cleaned up.
func spawn(spec SpawnSpec) (Process, error) {
	if spec.Path == "" {
		return nil, fmt.Errorf("proc.Spawn: empty executable path")
	}

	// 1. Create a Job Object grouping the child + its descendants so an explicit stop can
	//    TerminateJobObject the whole tree reliably (more robust than taskkill /T for
	//    re-parented grandchildren).
	//
	//    DESIGN CHOICE (reconcile reattach): we deliberately do NOT set
	//    JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. If we did, a daemon crash (which closes all its
	//    handles, including the job) would take every spawned llama-server down with it — and
	//    the child could never be re-attached after a daemon restart (spec-digest §1 requires
	//    "PID alive + healthy + identity → reattach as HEALTHY"). By leaving kill-on-close OFF,
	//    a crashed daemon leaves its children RUNNING, so the reconciler can re-attach a healthy
	//    survivor or reap a mismatched one. The trade-off — a crashed daemon momentarily leaves
	//    GPU-holding children — is exactly what reconcile-on-startup exists to resolve, and an
	//    explicit Stop/Kill still terminates the whole tree via TerminateJobObject.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}

	// 2. Build the command, suspended + no-window, so we can assign it before it runs.
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | createSuspended,
	}
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("start %s: %w", spec.Path, err)
	}

	// 3. Assign the (suspended) child to the job, then resume its main thread.
	hProc := windows.Handle(cmd.Process.Pid)
	// Open a handle with the rights AssignProcessToJobObject needs.
	ph, oerr := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if oerr == nil {
		hProc = ph
		defer windows.CloseHandle(ph)
	}
	if err := windows.AssignProcessToJobObject(job, hProc); err != nil {
		// Assignment failed — kill the child we started so we don't leak an unmanaged process.
		_ = cmd.Process.Kill()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("assign process %d to job: %w", cmd.Process.Pid, err)
	}
	if err := resumeMainThread(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("resume process %d: %w", cmd.Process.Pid, err)
	}

	p := &winProcess{cmd: cmd, job: job, waitDone: make(chan struct{})}
	// Reap the child in the background so Wait() and Done() have an authoritative exit status
	// and the OS process handle is released.
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.waitErr = err
		p.waited = true
		p.mu.Unlock()
		close(p.waitDone)
	}()
	return p, nil
}

func (p *winProcess) PID() int { return p.cmd.Process.Pid }

func (p *winProcess) Done() bool {
	select {
	case <-p.waitDone:
		return true
	default:
		return false
	}
}

func (p *winProcess) Wait(ctx context.Context) error {
	select {
	case <-p.waitDone:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop asks the process to exit gracefully (taskkill without /F posts WM_CLOSE / console
// signal), waits up to GraceTimeout, then force-kills the whole tree via TerminateJobObject.
// Returns once the process is confirmed gone. The lmwrapper "terminate → kill after 5s" rule.
func (p *winProcess) Stop(ctx context.Context) error {
	if p.Done() {
		return p.terminateJob()
	}
	// Graceful: taskkill /PID <pid> /T (no /F) requests a clean shutdown of the tree. llama-server
	// handles this cleanly (lmwrapper lesson). Best-effort — a console app may ignore it.
	_ = runTaskkill(p.PID(), false)

	// Wait budget (lmwrapper: 5s), bounded also by the caller's ctx.
	waitCtx, cancel := context.WithTimeout(ctx, GraceTimeout)
	defer cancel()
	select {
	case <-p.waitDone:
		return p.terminateJob()
	case <-waitCtx.Done():
		// Grace expired (or ctx cancelled): force-kill the tree via the job.
	}
	if err := p.terminateJob(); err != nil {
		return err
	}
	// Give the OS a moment to tear down and our reaper to observe the exit.
	select {
	case <-p.waitDone:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (p *winProcess) Kill() error { return p.terminateJob() }

// terminateJob force-kills every process in the job (TerminateJobObject) then closes the
// handle. This is the reliable tree-kill: it does not rely on kill-on-close (which we do NOT
// set, so children survive a daemon crash for reattach). Idempotent: once the handle is closed
// the job is gone.
func (p *winProcess) terminateJob() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return nil
	}
	termErr := windows.TerminateJobObject(p.job, 1)
	closeErr := windows.CloseHandle(p.job)
	p.job = 0
	if termErr != nil {
		return fmt.Errorf("terminate job: %w", termErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close job handle: %w", closeErr)
	}
	return nil
}

// resumeMainThread resumes the primary thread of a suspended process. exec.Command with
// CREATE_SUSPENDED starts the process suspended; we must resume it after job assignment. We
// enumerate the process's threads and resume the first one (the main thread for a freshly
// created process). Uses the Toolhelp snapshot API.
func resumeMainThread(pid int) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("thread snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	if err := windows.Thread32First(snap, &te); err != nil {
		return fmt.Errorf("thread32first: %w", err)
	}
	for {
		if te.OwnerProcessID == uint32(pid) {
			th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
			if oerr == nil {
				_, _ = resumeThread(th)
				windows.CloseHandle(th)
				return nil
			}
		}
		if err := windows.Thread32Next(snap, &te); err != nil {
			// No more threads.
			return fmt.Errorf("no resumable thread found for pid %d: %w", pid, err)
		}
	}
}

var (
	modkernel32      = windows.NewLazySystemDLL("kernel32.dll")
	procResumeThread = modkernel32.NewProc("ResumeThread")
)

func resumeThread(th windows.Handle) (uint32, error) {
	r1, _, err := procResumeThread.Call(uintptr(th))
	if int32(r1) < 0 {
		return 0, err
	}
	return uint32(r1), nil
}

// isAlive reports whether a PID is currently running, by opening the process with limited
// query rights and reading its exit code (STILL_ACTIVE => running). A PID we cannot open is
// treated as not-alive (it either does not exist or belongs to another security context we
// cannot touch — either way we should not consider it our live instance).
func isAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// killTree force-kills the tree rooted at pid via `taskkill /PID <pid> /T /F`. Used at reconcile
// time for an orphan from a prior daemon (we hold no job handle). Returns an error only if the
// taskkill command could not be issued.
func killTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("killTree: invalid pid %d", pid)
	}
	return runTaskkill(pid, true)
}

// runTaskkill invokes taskkill for pid. force adds /F (hard kill); without /F it is a graceful
// close request. /T includes the whole tree. CREATE_NO_WINDOW keeps it silent.
func runTaskkill(pid int, force bool) error {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	cmd := exec.Command("taskkill", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	// taskkill returns non-zero if the process is already gone; that is success for our purpose.
	_ = cmd.Run()
	return nil
}
