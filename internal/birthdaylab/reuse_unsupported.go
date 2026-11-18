//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package birthdaylab

import (
	"errors"
	"syscall"
)

var errUnsupportedPlatform = errors.New("birthday prototype: SO_REUSEPORT is not available on this platform")

func reuseControl(_, _ string, _ syscall.RawConn) error {
	return errUnsupportedPlatform
}
