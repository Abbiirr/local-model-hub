//go:build !windows

package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// otherProcess is the portable (non-Windows) fallback. It launches the child in its own process
// group (Setpgid) so we can signal the whole group, mirroring the Job-Object tree-kill on
// Windows. Graceful stop is SIGTERM then SIGKILL after GraceTimeout — the POSIX analog of the
// lmwrapper terminate→kill rule. This exists so the package builds and unit-tests on Linux CI;
// the reference box uses proc_windows.go.
type otherProcess struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	waitErr  error
	waited   bool
	waitDone chan struct{}
}

func spawn(spec SpawnSpec) (Process, error) {
	if spec.Path == "" {
		return nil, fmt.Errorf("proc.Spawn: empty executable path")
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.Dir
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Path, err)
	}
	p := &otherProcess{cmd: cmd, waitDone: make(chan struct{})}
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

func (p *otherProcess) PID() int { return p.cmd.Process.Pid }

func (p *otherProcess) Done() bool {
	select {
	case <-p.waitDone:
		return true
	default:
		return false
	}
}

func (p *otherProcess) Wait(ctx context.Context) error {
	select {
	case <-p.waitDone:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *otherProcess) Stop(ctx context.Context) error {
	if p.Done() {
		return nil
	}
	pgid := -p.cmd.Process.Pid // negative pid = process group
	_ = syscall.Kill(pgid, syscall.SIGTERM)

	waitCtx, cancel := context.WithTimeout(ctx, GraceTimeout)
	defer cancel()
	select {
	case <-p.waitDone:
		return nil
	case <-waitCtx.Done():
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
	select {
	case <-p.waitDone:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func (p *otherProcess) Kill() error {
	return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}

func isAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes existence without affecting the process.
	return proc.Signal(syscall.Signal(0)) == nil
}

func killTree(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("killTree: invalid pid %d", pid)
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}
