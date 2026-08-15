//go:build !windows

package winproc

import "os/exec"

// Hide is a no-op off Windows: there is no console-window-flash concern on Linux/macOS, and
// SysProcAttr has no equivalent field. Kept so callers need no build tags of their own.
func Hide(cmd *exec.Cmd) {}
