//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package burrow

import "syscall"

func reuseControl(_, _ string, _ syscall.RawConn) error {
	return ErrUnsupportedPlatform
}
