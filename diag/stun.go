// Package diag provides NAT diagnostics: STUN NAT type detection and TCP NAT
// mapping behavior probing.
//
// The CLI and the mobile bindings share this implementation. Each Run*
// function returns both a structured result and the full human-readable
// report (Result.Detail), which callers can print as is.
package diag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/getkiko/burrow/internal/netiface"

	natstun "github.com/ccding/go-stun/stun"

	"github.com/getkiko/burrow"
)

const (
	// DefaultSTUNServer and DefaultSTUNTimeout are the defaults shared by the
	// CLI and the mobile bindings.
	DefaultSTUNServer  = "stun.miwifi.com:3478"
	DefaultSTUNTimeout = 30 * time.Second

	stunMappingTimeout = 5 * time.Second
)

// STUNConfig is the input of a STUN test.
type STUNConfig struct {
	Server  string        // STUN server address; empty means DefaultSTUNServer
	Iface   string        // outbound interface; empty lets the kernel route
	Timeout time.Duration // timeout of the full test; <=0 means DefaultSTUNTimeout
}

// STUNResult is the outcome of a STUN test.
//
// Detail is the full human-readable report. The structured fields are for
// programmatic checks, in particular IsSymmetric, which the server-side NAT
// self-check relies on.
type STUNResult struct {
	NATType     string // RFC-style type name, e.g. "Symmetric NAT"
	NATLabel    string // descriptive label with the NAT1-4 name, e.g. "Symmetric (NAT4)"
	PublicAddr  string // public UDP mapped address
	IsSymmetric bool   // whether the NAT is symmetric (NAT4)
	Detail      string // full text report
}

// RunSTUN runs STUN mapping and filtering behavior discovery.
//
// Even when it returns an error, STUNResult.Detail may contain the partial
// report produced so far, which callers can show to help troubleshooting.
func RunSTUN(parent context.Context, cfg STUNConfig) (STUNResult, error) {
	var buf bytes.Buffer
	result := STUNResult{}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultSTUNTimeout
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	server, err := NormalizeSTUNServer(cfg.Server)
	if err != nil {
		return result, err
	}
	serverAddr, err := resolveSTUNServerAddr(ctx, server)
	if err != nil {
		result.Detail = buf.String()
		return result, fmt.Errorf("resolve STUN server %q: %w", server, err)
	}

	localIP, err := resolveSTUNLocalIPv4(ctx, cfg.Iface, serverAddr)
	if err != nil {
		result.Detail = buf.String()
		return result, fmt.Errorf("determine outbound IPv4 for STUN: %w", err)
	}
	// Bind to the device as well as the source IP: when a system-wide tun
	// proxy has hijacked the default route (common on phones), the kernel
	// still hands packets to the tun device if only the source IP is set,
	// and the test would report the proxy egress's NAT type instead of the
	// local one. A wrong NAT type misleads troubleshooting, which is worse
	// than no result.
	listenConfig := &net.ListenConfig{Control: netiface.BindControl(cfg.Iface)}
	packetConn, err := listenConfig.ListenPacket(ctx, "udp4", (&net.UDPAddr{IP: localIP}).String())
	if err != nil {
		result.Detail = buf.String()
		return result, fmt.Errorf("create STUN UDP socket: %w", err)
	}
	conn, ok := packetConn.(*net.UDPConn)
	if !ok {
		_ = packetConn.Close()
		result.Detail = buf.String()
		return result, fmt.Errorf("create STUN UDP socket: not a UDP connection")
	}
	defer conn.Close()
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)

	interfaceName := cfg.Iface
	if interfaceName == "" {
		interfaceName = InterfaceForIPv4(localIP.String())
	}
	fmt.Fprintf(&buf, "[stun] server=%s interface=%s local_ip=%s local_port=%d\n",
		server, interfaceName, localIP, conn.LocalAddr().(*net.UDPAddr).Port)

	queryTimeout := stunMappingTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < queryTimeout {
		queryTimeout = time.Until(deadline)
	}
	mappedAddr, err := awaitSTUNOperation(ctx, func() (*net.UDPAddr, error) {
		return burrow.QueryUDPMappedAddr(conn, server, queryTimeout)
	})
	if err != nil {
		result.Detail = buf.String()
		return result, stunRunError(ctx, timeout, fmt.Errorf("query public mapping: %w", err))
	}
	result.PublicAddr = mappedAddr.String()
	fmt.Fprintf(&buf, "[stun] public UDP mapping=%s\n", mappedAddr)
	fmt.Fprintln(&buf, "[stun] running RFC 3489/5780 mapping and filtering behavior discovery ...")

	client := natstun.NewClientWithConnection(conn)
	client.SetServerAddr(server)
	behavior, behaviorErr := awaitSTUNOperation(ctx, client.BehaviorTest)
	if behavior != nil {
		printSTUNBehavior(&buf, *behavior)
		normalType := behavior.NormalType()
		result.NATType, result.NATLabel = stunNormalTypeFromString(normalType)
		result.IsSymmetric = isSymmetricNATType(normalType)
	}
	result.Detail = buf.String()

	if behaviorErr == nil {
		return result, nil
	}
	if errors.Is(behaviorErr, natstun.ErrBehaviorDiscoveryUnsupported) {
		fmt.Fprintln(&buf, "[type] this STUN server answers mapping queries but lacks the alternate IP and port needed for type detection.")
		fmt.Fprintln(&buf, "[hint] retry with a STUN server that supports RFC 5780 OTHER-ADDRESS.")
		result.Detail = buf.String()
		return result, nil
	}
	return result, stunRunError(ctx, timeout, fmt.Errorf("NAT type detection: %w", behaviorErr))
}

// NormalizeSTUNServer normalizes a STUN server address: it strips the
// stun:// and stuns:// prefixes and surrounding whitespace, and adds port
// 3478 when no port is given.
func NormalizeSTUNServer(value string) (string, error) {
	server := strings.TrimSpace(value)
	server = strings.TrimPrefix(server, "stun://")
	server = strings.TrimPrefix(server, "stuns://")
	if server == "" {
		return "", fmt.Errorf("stun server must not be empty")
	}
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server, nil
	}
	trimmedIP := strings.Trim(server, "[]")
	if ip := net.ParseIP(trimmedIP); ip != nil {
		return net.JoinHostPort(ip.String(), "3478"), nil
	}
	if !strings.Contains(server, ":") {
		return net.JoinHostPort(server, "3478"), nil
	}
	return "", fmt.Errorf("invalid STUN server address %q", value)
}

func resolveSTUNServerAddr(ctx context.Context, server string) (*net.UDPAddr, error) {
	host, portText, err := net.SplitHostPort(server)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %q out of range 1-65535", portText)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return nil, fmt.Errorf("the test requires an IPv4 STUN server")
		}
		return &net.UDPAddr{IP: append(net.IP(nil), ip.To4()...), Port: port}, nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if ip := address.AsSlice(); net.IP(ip).To4() != nil {
			return &net.UDPAddr{IP: append(net.IP(nil), net.IP(ip).To4()...), Port: port}, nil
		}
	}
	return nil, fmt.Errorf("host %s has no IPv4 address", host)
}

// resolveSTUNLocalIPv4 looks up the IPv4 address of the named interface
// (through netiface, which avoids Android's netlink restriction). Without an
// interface it asks the kernel's routing for the outbound address.
func resolveSTUNLocalIPv4(ctx context.Context, interfaceName string, server *net.UDPAddr) (net.IP, error) {
	if interfaceName != "" {
		addr, err := netiface.IPv4ByName(interfaceName)
		if err != nil {
			return nil, err
		}
		return append(net.IP(nil), addr.AsSlice()...), nil
	}

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp4", server.String())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP.To4() == nil {
		return nil, fmt.Errorf("kernel routing returned no IPv4 address")
	}
	return append(net.IP(nil), local.IP.To4()...), nil
}

func printSTUNBehavior(output io.Writer, behavior natstun.NATBehavior) {
	mappingCode, mappingName := stunBehaviorName(behavior.MappingType, true)
	filteringCode, filteringName := stunBehaviorName(behavior.FilteringType, false)
	if mappingCode != "" {
		fmt.Fprintf(output, "[mapping] %s - %s\n", mappingCode, mappingName)
	}
	if filteringCode != "" {
		fmt.Fprintf(output, "[filtering] %s - %s\n", filteringCode, filteringName)
	}
	name, label := stunNormalTypeFromString(behavior.NormalType())
	fmt.Fprintf(output, "[NAT type] %s - %s\n", name, label)
}

func stunBehaviorName(kind natstun.BehaviorType, mapping bool) (string, string) {
	if mapping {
		switch kind {
		case natstun.BehaviorTypeEndpoint:
			return "EIM", "endpoint-independent mapping"
		case natstun.BehaviorTypeAddr:
			return "ADM", "address-dependent mapping"
		case natstun.BehaviorTypeAddrAndPort:
			return "APDM", "address and port-dependent mapping"
		}
		return "", "mapping behavior undetermined"
	}
	switch kind {
	case natstun.BehaviorTypeEndpoint:
		return "EIF", "endpoint-independent filtering"
	case natstun.BehaviorTypeAddr:
		return "ADF", "address-dependent filtering"
	case natstun.BehaviorTypeAddrAndPort:
		return "APDF", "address and port-dependent filtering"
	default:
		return "", "filtering behavior undetermined"
	}
}

// stunNormalTypeFromString maps go-stun's NormalType() string to a type name
// and a descriptive label.
func stunNormalTypeFromString(normalType string) (string, string) {
	switch normalType {
	case "Full cone NAT":
		return "Full Cone NAT", "Full cone (NAT1)"
	case "Restricted cone NAT":
		return "Restricted Cone NAT", "Address-restricted cone (NAT2)"
	case "Port Restricted cone NAT":
		return "Port Restricted Cone NAT", "Port-restricted cone (NAT3)"
	case "Symmetric NAT":
		return "Symmetric NAT", "Symmetric (NAT4)"
	case "Open Internet (no NAT)":
		return "Open Internet", "No NAT"
	case natstun.SymmetricUDPFirewall.String():
		return "Symmetric UDP Firewall", "Symmetric UDP firewall"
	default:
		return "Behavior Combination", "Mixed NAT behavior"
	}
}

// isSymmetricNATType reports whether the NAT is symmetric (NAT4).
//
// It decides whether the server side can act as the punching listener: the
// birthday strategy needs a stable port mapping on the listener side, and a
// symmetric NAT allocates a new port for every new destination.
func isSymmetricNATType(normalType string) bool {
	return normalType == "Symmetric NAT"
}

func stunRunError(ctx context.Context, timeout time.Duration, err error) error {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return fmt.Errorf("STUN type detection exceeded %s", timeout)
	case context.Canceled:
		return context.Canceled
	default:
		return err
	}
}

type stunOperationResult[T any] struct {
	value T
	err   error
}

func awaitSTUNOperation[T any](ctx context.Context, operation func() (T, error)) (T, error) {
	result := make(chan stunOperationResult[T], 1)
	go func() {
		value, err := operation()
		result <- stunOperationResult[T]{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case completed := <-result:
		return completed.value, completed.err
	}
}

// InterfaceForIPv4 returns the name of the local interface that owns the
// given IPv4 address, or "unknown" if it cannot be found.
//
// This requires enumeration, which has no ioctl alternative and usually fails
// on Android because of netlink permissions. Failure degrades to "unknown"
// rather than an error: the name is informational only and must not make the
// diagnostic fail.
func InterfaceForIPv4(target string) string {
	return netiface.NameForIPv4(target)
}
