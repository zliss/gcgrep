//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// spawnDaemon starts the daemon detached from the console so it survives
// the client's terminal closing and opens no window.
func spawnDaemon(exe string) error {
	cmd := exec.Command(exe, "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
	return cmd.Start()
}
