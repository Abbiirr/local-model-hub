//go:build windows

package trayapp

import (
	"testing"
)

// The tray has no console of its own (-H=windowsgui), so any console child it spawns would get a
// brand new visible console window allocated by Windows. Every process the tray starts must
// therefore carry CREATE_NO_WINDOW; this guards the one place that decides it.
func TestHiddenCommandSuppressesConsoleWindow(t *testing.T) {
	const createNoWindow = 0x08000000

	command := hiddenCommand(`C:\somewhere`, "localhub.exe", "-config", "configs/config.yaml")
	if command.Dir != `C:\somewhere` {
		t.Fatalf("working directory = %q", command.Dir)
	}
	if command.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil: the child would open a console window")
	}
	if !command.SysProcAttr.HideWindow {
		t.Error("HideWindow is false")
	}
	if command.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Errorf("CreationFlags = %#x, want CREATE_NO_WINDOW set", command.SysProcAttr.CreationFlags)
	}
}
