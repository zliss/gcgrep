//go:build darwin || linux || freebsd

package ipc

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"
)

func socketPath() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

// Listen binds the daemon endpoint. A stale socket file from a crashed
// daemon is detected by dialing it first and removed if dead.
func Listen() (net.Listener, error) {
	path, err := socketPath()
	if err != nil {
		return nil, err
	}
	if c, derr := net.DialTimeout("unix", path, 200*time.Millisecond); derr == nil {
		c.Close()
		return nil, errors.New("daemon already running")
	}
	_ = os.Remove(path)
	return net.Listen("unix", path)
}

func Dial(timeout time.Duration) (net.Conn, error) {
	path, err := socketPath()
	if err != nil {
		return nil, err
	}
	return net.DialTimeout("unix", path, timeout)
}
