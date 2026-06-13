//go:build !windows

package daemon

import "syscall"

// lowerPriority drops the daemon to background priority (nice +10) so
// indexing never competes with the user's foreground work.
func lowerPriority() error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
}
