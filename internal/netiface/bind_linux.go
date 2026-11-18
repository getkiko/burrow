//go:build linux

package netiface

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindControl returns a control callback that pins a socket to the named
// interface.
//
// It tries SO_BINDTOIFINDEX first and falls back to SO_BINDTODEVICE. The
// order matters on Android:
//
//   - SO_BINDTODEVICE requires CAP_NET_RAW. Android app processes do not have
//     it, so it always fails with EPERM. Its only advantage is that old
//     kernels support it.
//   - SO_BINDTOIFINDEX (Linux 5.7+) does not require CAP_NET_RAW, so ordinary
//     app processes can use it. Kernels older than 5.7 return ENOPROTOOPT.
//
// Trying BINDTODEVICE first would always hit EPERM on Android and silently
// end up unbound: no error, but traffic still follows the default route.
// Trying BINDTOIFINDEX first succeeds on modern Android kernels; old kernels
// get ENOPROTOOPT and fall back to BINDTODEVICE, and processes on such
// devices usually have looser permissions anyway.
//
// Both options have the same effect: the kernel bypasses the routing table
// and sends and receives on the given interface.
//
// If both fail for permission reasons the connection proceeds unbound. A
// failed bind only means traffic might use the wrong interface, whereas
// returning an error would prevent the connection entirely, which is worse;
// source-IP binding remains as a fallback.
func bindControl(name string) (func(network, address string, c syscall.RawConn) error, bool) {
	return func(network, address string, conn syscall.RawConn) error {
		var innerErr error
		if ctrlErr := conn.Control(func(fd uintptr) {
			// Bind by index first: it needs no CAP_NET_RAW and is the
			// only path that works for Android app processes.
			if index, err := IndexByName(name); err == nil {
				innerErr = unix.SetsockoptInt(
					int(fd), unix.SOL_SOCKET, unix.SO_BINDTOIFINDEX, index,
				)
				if innerErr == nil {
					return
				}
			} else {
				innerErr = err
			}
			// Kernels < 5.7 reject SO_BINDTOIFINDEX (ENOPROTOOPT); fall
			// back to binding by name.
			if fallbackErr := unix.SetsockoptString(
				int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name,
			); fallbackErr == nil {
				innerErr = nil
			}
		}); ctrlErr != nil {
			return ctrlErr
		}
		if innerErr != nil && !isPermissionDenied(innerErr) {
			// Errors other than permission problems (for example a
			// missing interface) are still worth surfacing.
			return innerErr
		}
		return nil
	}, true
}

// isPermissionDenied reports whether err means the environment does not
// allow binding (no permission or unsupported option).
//
// Such failures are expected (Android app processes lack CAP_NET_RAW, old
// kernels lack a sockopt) and must not fail the connection.
func isPermissionDenied(err error) bool {
	return errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES) ||
		errors.Is(err, unix.ENOPROTOOPT)
}
