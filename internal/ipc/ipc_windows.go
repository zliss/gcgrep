//go:build windows

package ipc

import (
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
)

// pipeName is per-user so concurrent sessions do not collide; usernames
// can contain spaces etc. but pipe names accept them.
func pipeName() string {
	user := os.Getenv("USERNAME")
	if user == "" {
		user = "default"
	}
	return `\\.\pipe\gcgrep-` + user
}

func Listen() (net.Listener, error) {
	return winio.ListenPipe(pipeName(), nil)
}

func Dial(timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(pipeName(), &timeout)
}
