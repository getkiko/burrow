//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package burrow

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reuseControl sets SO_REUSEADDR and SO_REUSEPORT on a socket.
//
// TCP hole punching reuses one local port for address reflection, listening
// and dialing. Without these options the second bind to that port fails with
// EADDRINUSE.
func reuseControl(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			serr = err
			return
		}
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return serr
}
