//go:build !windows

package monitor

import "syscall"

// processAlive checks whether pid exists.
//
// On Unix we can use "signal 0": it does not actually send a signal,
// only performs existence/permission check.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
