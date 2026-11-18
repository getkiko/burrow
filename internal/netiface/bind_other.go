//go:build !linux

package netiface

import "syscall"

// bindControl returns supported=false on non-Linux platforms, because
// SO_BINDTOIFINDEX is Linux-specific.
//
// This lets callers such as the tunnel package skip platform checks: on
// false they simply leave burrow.Config.Control unset. macOS and Windows do
// not have the typical Android case of a tun device hijacking the default
// route, so source-IP binding (burrow.Config.BindInterface) is sufficient.
func bindControl(string) (func(network, address string, c syscall.RawConn) error, bool) {
	return nil, false
}
