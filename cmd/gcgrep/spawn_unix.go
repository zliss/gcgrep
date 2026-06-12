//go:build darwin || linux || freebsd

package main

import (
	"os/exec"
	"syscall"
)

// spawnDaemon starts the daemon detached in its own session so it
// survives the client's terminal closing.
func spawnDaemon(exe string) error {
	cmd := exec.Command(exe, "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
