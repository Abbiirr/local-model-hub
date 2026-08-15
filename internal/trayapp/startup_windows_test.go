//go:build windows

package trayapp

import "testing"

func TestStartupApprovedEnabled(t *testing.T) {
	if !startupApprovedEnabled(nil) || !startupApprovedEnabled([]byte{0x02}) {
		t.Fatal("missing and enabled StartupApproved states must be enabled")
	}
	if startupApprovedEnabled([]byte{0x03, 0, 0}) {
		t.Fatal("Task Manager disabled StartupApproved state was treated as enabled")
	}
}
