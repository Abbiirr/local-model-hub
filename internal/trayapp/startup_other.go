//go:build !windows

package trayapp

// StartupEnabled is false on non-Windows builds; the native tray is Windows-only.
func (c *Controller) StartupEnabled() bool { return false }

// SetStartupEnabled is a no-op on non-Windows builds used only to keep cross-platform tests and
// package builds available.
func (c *Controller) SetStartupEnabled(enabled bool) error { return nil }
