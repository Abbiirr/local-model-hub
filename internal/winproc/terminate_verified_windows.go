//go:build windows

package winproc

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// TerminateVerified terminates pid only when its executable is one of allowedPaths.
func TerminateVerified(pid int, allowedPaths []string) error {
	if pid <= 0 {
		return fmt.Errorf("invalid LocalHub daemon pid %d", pid)
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE,
		false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open LocalHub daemon pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	const maxWindowsPath = 32768
	buffer := make([]uint16, maxWindowsPath)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return fmt.Errorf("read executable for LocalHub daemon pid %d: %w", pid, err)
	}
	actual := filepath.Clean(windows.UTF16ToString(buffer[:size]))
	verified := false
	for _, allowed := range allowedPaths {
		absolute, err := filepath.Abs(allowed)
		if err == nil && strings.EqualFold(actual, filepath.Clean(absolute)) {
			verified = true
			break
		}
	}
	if !verified {
		return fmt.Errorf("refusing to terminate pid %d: executable %q is not a LocalHub daemon from this checkout", pid, actual)
	}
	if err := windows.TerminateProcess(handle, 0); err != nil {
		return fmt.Errorf("terminate LocalHub daemon pid %d: %w", pid, err)
	}
	return nil
}
