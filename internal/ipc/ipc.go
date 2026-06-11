// Package ipc abstracts the daemon transport: unix domain sockets on
// macOS/Linux, named pipes on Windows. No TCP port is ever used.
package ipc

import (
	"os"
	"path/filepath"
)

// CacheDir returns the gcgrep state directory (index files, socket, logs),
// creating it if needed.
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "gcgrep")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
