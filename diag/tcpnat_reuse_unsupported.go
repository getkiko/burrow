//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package diag

import (
	"errors"
	"syscall"
)

var errUnsupportedPlatform = errors.New("tcpnat: SO_REUSEPORT is not available on this platform")

func reuseControl(_, _ string, _ syscall.RawConn) error {
	return errUnsupportedPlatform
}
