//go:build windows

// Package winproc centralizes the "do not flash a console window" flag for every child process
// the daemon spawns on Windows.
//
// This exists because the flag was previously applied in exactly ONE place (internal/hardware,
// for nvidia-smi) while at least six other spawn sites had no such protection: the llama.cpp
// capability probe, the adopt path's powershell/nvidia-smi calls, the vLLM docker CLI calls,
// and taskkill. The capability probe is the worst offender — the dashboard polls /api/engines,
// and each poll runs `llama-server --help` once PER DETECTED BUILD, so a box with three
// llama.cpp installs flashes three console windows on every refresh.
//
// The daemon itself is linked -H=windowsgui and has no console, which makes this MORE important,
// not less: a GUI-subsystem parent spawning a console child causes Windows to allocate a brand
// new console window for the child — visible on screen, and closable by anyone walking past.
package winproc

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW. The child runs with no console allocated at all.
const createNoWindow = 0x08000000

// Hide marks cmd so it spawns without a console window. Call it BEFORE Start/Run/Output —
// SysProcAttr is read at spawn time, so setting it afterwards silently does nothing.
//
// Safe on a nil cmd and safe to call twice. It overwrites SysProcAttr wholesale, so a caller
// that needs other SysProcAttr fields (e.g. CREATE_SUSPENDED for job-object assignment, as
// internal/instances/proc does) must set those itself rather than using this helper.
func Hide(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
