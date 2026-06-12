//go:build windows

package daemon

import "syscall"

// lowerPriority drops the daemon to BELOW_NORMAL_PRIORITY_CLASS so
// indexing never competes with the user's foreground work.
func lowerPriority() error {
	const belowNormalPriorityClass = 0x00004000
	setPriorityClass := syscall.NewLazyDLL("kernel32.dll").NewProc("SetPriorityClass")
	handle, err := syscall.GetCurrentProcess()
	if err != nil {
		return err
	}
	r, _, callErr := setPriorityClass.Call(uintptr(handle), uintptr(belowNormalPriorityClass))
	if r == 0 {
		return callErr
	}
	return nil
}
