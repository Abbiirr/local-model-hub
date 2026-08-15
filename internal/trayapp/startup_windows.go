//go:build windows

package trayapp

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	windowsRunKeyPath             = `Software\Microsoft\Windows\CurrentVersion\Run`
	windowsRunValue               = "LocalHub Compass"
	windowsStartupApprovedKeyPath = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run`
)

// StartupEnabled reports whether Windows is configured to launch this tray application for the
// current user. The HKCU Run entry is deliberately used instead of a Scheduled Task so Windows
// exposes LocalHub in Task Manager's Startup apps page and the operator can disable it there.
func (c *Controller) StartupEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsRunKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	value, _, err := key.GetStringValue(windowsRunValue)
	if err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(value), c.startupCommand()) {
		return false
	}
	approved, err := registry.OpenKey(registry.CURRENT_USER, windowsStartupApprovedKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer approved.Close()
	state, _, err := approved.GetBinaryValue(windowsRunValue)
	if err != nil {
		return true
	}
	return startupApprovedEnabled(state)
}

// SetStartupEnabled updates the current-user Windows Startup Apps registration. It does not start
// or stop the current process; it changes only what happens at the next interactive logon.
func (c *Controller) SetStartupEnabled(enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, windowsRunKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open Windows Startup Apps registration: %w", err)
	}
	defer key.Close()
	if !enabled {
		if err := key.DeleteValue(windowsRunValue); err != nil && err != registry.ErrNotExist {
			return fmt.Errorf("disable LocalHub startup: %w", err)
		}
		return nil
	}
	if err := key.SetStringValue(windowsRunValue, c.startupCommand()); err != nil {
		return fmt.Errorf("enable LocalHub startup: %w", err)
	}
	// Task Manager keeps its disabled marker after the Run value is rewritten. Remove that marker
	// when the operator explicitly re-enables startup from LocalHub; absence means enabled.
	if approved, err := registry.OpenKey(registry.CURRENT_USER, windowsStartupApprovedKeyPath, registry.SET_VALUE); err == nil {
		_ = approved.DeleteValue(windowsRunValue)
		approved.Close()
	}
	return nil
}

func startupApprovedEnabled(value []byte) bool {
	// Windows StartupApproved values use 0x02 for enabled and 0x03 for disabled. Missing/unknown
	// values are treated as enabled because the Run registration itself remains authoritative.
	return len(value) == 0 || value[0] != 0x03
}

func (c *Controller) startupCommand() string {
	executable, err := os.Executable()
	if err != nil || strings.TrimSpace(executable) == "" {
		executable = "localhub-tray.exe"
	}
	return quoteWindowsArgument(executable) + " -repo " + quoteWindowsArgument(c.repoRoot)
}

func quoteWindowsArgument(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
