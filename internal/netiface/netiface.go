// Package netiface provides cross-platform lookup of network interfaces by
// name.
//
// SELinux denies non-root Android processes access to the netlink routing
// table ("netlinkrib: permission denied"). On Linux the standard library's
// net.InterfaceByName and net.Interfaces are implemented over netlink, so
// on Android they fail with a permission error.
//
// This package instead uses ioctl (SIOCGIFINDEX / SIOCGIFADDR) on an AF_INET
// socket to look up an interface's index and IPv4 address by name, which
// does not touch netlink and is not subject to that restriction.
//
// ioctl can only look up a known name; it cannot enumerate interfaces.
// Enumeration (for example listing available interfaces in an error
// message) still goes through net.Interfaces and will fail on Android, so
// callers must treat enumeration failure as non-fatal and degrade to a
// message without the list.
package netiface

import (
	"net"
	"net/netip"
	"syscall"
)

// IndexByName returns the index of the named interface, for socket APIs
// such as SO_BINDTOIFINDEX that take an index.
//
// It uses ioctl where supported (Linux only, bypassing netlink). On other
// platforms it falls back to net.InterfaceByName, which has no permission
// problem there.
func IndexByName(name string) (int, error) {
	if index, supported, err := systemIndexByName(name); supported {
		if err == nil {
			return index, nil
		}
		// ioctl is supported but failed (for example the interface does
		// not exist). Return that error; the standard library would look
		// up the same thing, so retrying is pointless.
		return 0, err
	}
	device, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return device.Index, nil
}

// IPv4ByName returns the first non-loopback IPv4 address of the named
// interface.
//
// It uses ioctl where supported and otherwise falls back to
// net.InterfaceByName and Addrs.
func IPv4ByName(name string) (netip.Addr, error) {
	if address, supported, err := systemIPv4ByName(name); supported {
		if err == nil {
			return address, nil
		}
		return netip.Addr{}, err
	}
	device, err := net.InterfaceByName(name)
	if err != nil {
		return netip.Addr{}, err
	}
	addrs, err := device.Addrs()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if v4 := ip.To4(); v4 != nil && !ip.IsLoopback() {
			parsed, valid := netip.AddrFromSlice(v4)
			if valid {
				return parsed, nil
			}
		}
	}
	return netip.Addr{}, &net.OpError{Op: "ipv4", Net: name, Err: errNoIPv4{name: name}}
}

// BindControl returns a control callback that pins a socket to the named
// interface. It can be assigned to net.Dialer.Control,
// net.ListenConfig.Control or burrow.Config.Control. It returns nil if name
// is empty or the platform does not support device binding.
//
// Source-IP binding alone is not enough when the host runs a system-wide
// tun proxy: the default route is hijacked, so the kernel still hands
// packets to the tun device and traffic loops back through the proxy.
// Binding to the device makes the kernel bypass the routing table and use
// the physical interface directly.
//
// The signaling (WebSocket) connection needs this in particular. With only
// BindInterface set, signaling traffic on a phone running a tun proxy is
// captured by the proxy; besides extra latency and timeouts, the server
// then sees the proxy's egress IP, so the punch candidates are wrong from
// the start.
func BindControl(name string) func(network, address string, c syscall.RawConn) error {
	if name == "" {
		return nil
	}
	control, supported := bindControl(name)
	if !supported {
		return nil
	}
	return control
}

type errNoIPv4 struct{ name string }

func (e errNoIPv4) Error() string { return "interface " + e.name + " has no usable IPv4 address" }

// ListNames returns the names of all local interfaces.
//
// This is the one operation with no ioctl alternative, since ioctl cannot
// enumerate. It will usually fail on Android (netlink permission), so
// callers must treat failure as "no list available" rather than aborting,
// for example by showing an error message without the list of candidates.
func ListNames() ([]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(interfaces))
	for _, iface := range interfaces {
		names = append(names, iface.Name)
	}
	return names, nil
}

// NameForIPv4 returns the name of the local interface that owns the given
// IPv4 address, or "unknown" if none matches or enumeration fails.
//
// It relies on enumeration (net.Interfaces), which has no ioctl alternative
// on Android. It never returns an error: the result is only informational
// for diagnostics and must not make the diagnostic itself fail.
func NameForIPv4(target string) string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "unknown"
	}
	for _, device := range interfaces {
		addresses, err := device.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.String() == target {
				return device.Name
			}
		}
	}
	return "unknown"
}
