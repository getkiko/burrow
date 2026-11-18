//go:build !linux

package netiface

import "net/netip"

// systemIndexByName has no ioctl implementation outside Linux (SIOCGIFINDEX
// is Linux-specific). supported=false tells the caller to fall back to the
// standard net package; macOS and Windows do not have Android's netlink
// permission restriction.
func systemIndexByName(string) (int, bool, error) {
	return 0, false, nil
}

func systemIPv4ByName(string) (netip.Addr, bool, error) {
	return netip.Addr{}, false, nil
}
