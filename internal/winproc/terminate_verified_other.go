//go:build !windows

package winproc

import "fmt"

func TerminateVerified(pid int, _ []string) error {
	return fmt.Errorf("verified LocalHub daemon termination is unsupported on this platform (pid %d)", pid)
}
