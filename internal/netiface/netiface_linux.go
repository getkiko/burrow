//go:build linux

package netiface

import (
	"fmt"
	"net/netip"

	"golang.org/x/sys/unix"
)

// systemIndexByName looks up an interface index by name with SIOCGIFINDEX.
//
// It uses an AF_INET socket ioctl instead of netlink: SELinux denies
// non-root Android processes access to the netlink routing table
// ("netlinkrib: permission denied"), but not this ioctl.
func systemIndexByName(name string) (int, bool, error) {
	request, err := interfaceIoctl(name, unix.SIOCGIFINDEX)
	if err != nil {
		return 0, true, err
	}
	index := int(request.Uint32())
	if index <= 0 {
		return 0, true, fmt.Errorf("interface %q has invalid index %d", name, index)
	}
	return index, true, nil
}

// systemIPv4ByName looks up an interface's IPv4 address by name with
// SIOCGIFADDR.
func systemIPv4ByName(name string) (netip.Addr, bool, error) {
	request, err := interfaceIoctl(name, unix.SIOCGIFADDR)
	if err != nil {
		return netip.Addr{}, true, err
	}
	addressBytes, err := request.Inet4Addr()
	if err != nil {
		return netip.Addr{}, true, err
	}
	address, valid := netip.AddrFromSlice(addressBytes)
	if !valid {
		return netip.Addr{}, true, fmt.Errorf("interface %q has an invalid IPv4 address", name)
	}
	return address.Unmap(), true, nil
}

func interfaceIoctl(name string, request uint) (*unix.Ifreq, error) {
	socket, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(socket)
	interfaceRequest, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	if err := unix.IoctlIfreq(socket, request, interfaceRequest); err != nil {
		return nil, err
	}
	return interfaceRequest, nil
}
