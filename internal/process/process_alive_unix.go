//go:build !windows

package process

import "syscall"

// ProcessAlive checks whether pid exists.
//
// On Unix we can use "signal 0": it does not actually send a signal,
// only performs existence/permission check.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
