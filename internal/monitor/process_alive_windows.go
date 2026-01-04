//go:build windows

package monitor

import (
	"golang.org/x/sys/windows"
)

// STILL_ACTIVE is the Windows "process is still running" exit code used by GetExitCodeProcess.
// MS docs state it is 259.
const stillActive uint32 = 259

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// If we cannot open the process, treat as not alive.
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
