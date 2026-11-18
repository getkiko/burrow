//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package birthdaylab

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func reuseControl(_, _ string, conn syscall.RawConn) error {
	var socketErr error
	if err := conn.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			socketErr = err
			return
		}
		socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return socketErr
}
