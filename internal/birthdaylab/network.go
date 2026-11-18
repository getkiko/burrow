package birthdaylab

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/diag"
	"github.com/getkiko/burrow/internal/netiface"
	"github.com/getkiko/burrow/internal/signalproto"
)

const websocketTimeout = 8 * time.Second

type heldWebSocket struct {
	conn      *websocket.Conn
	transport *http.Transport
}

// MappingObservation records one public endpoint observed by a Signal NAT
// probe. All probes originate from StableEndpoint.LocalAddr.
type MappingObservation struct {
	TargetAddr string
	PublicAddr string
}

// StableEndpoint keeps address-reflection connections alive from one local TCP
// port. Matching public endpoints establish the server-side premise required
// by the one-dimensional birthday experiment.
type StableEndpoint struct {
	LocalAddr    string
	PublicAddr   string
	Observations []MappingObservation
	holds        []heldWebSocket
}

// FixedStableEndpoint describes a host whose public endpoint is known out of
// band, such as a cloud VM with one-to-one EIP mapping. It enables a controlled
// stable-mapping baseline without using the local Signal server as a reflector.
func FixedStableEndpoint(bindIP string, localPort int, publicAddress string) (*StableEndpoint, error) {
	if net.ParseIP(bindIP) == nil || localPort <= 0 || localPort > 65535 {
		return nil, fmt.Errorf("invalid fixed local endpoint: %s:%d", bindIP, localPort)
	}
	publicIP, publicPort, err := net.SplitHostPort(publicAddress)
	if err != nil || net.ParseIP(publicIP) == nil {
		return nil, fmt.Errorf("invalid fixed public endpoint: %q", publicAddress)
	}
	port, err := strconv.Atoi(publicPort)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid fixed public port: %q", publicPort)
	}
	return &StableEndpoint{
		LocalAddr:  net.JoinHostPort(bindIP, strconv.Itoa(localPort)),
		PublicAddr: publicAddress,
	}, nil
}

func (s *StableEndpoint) Close() {
	for _, hold := range s.holds {
		_ = hold.conn.Close(websocket.StatusNormalClosure, "experiment complete")
		hold.transport.CloseIdleConnections()
	}
}

func ParsePorts(value string) ([]int, error) {
	seen := make(map[int]struct{})
	var ports []int
	for _, field := range strings.Split(value, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		port, err := strconv.Atoi(field)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("invalid port %q", field)
		}
		if _, exists := seen[port]; exists {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("port list is empty")
	}
	return ports, nil
}

// ParseSignalAddresses parses a list of IPv4 Signal endpoints. Every endpoint
// carries its own optional path, for example 203.0.113.10:9960/signal.
func ParseSignalAddresses(value string) ([]string, error) {
	fields := strings.FieldsFunc(value, func(char rune) bool {
		return char == ',' || char == ';' || unicode.IsSpace(char)
	})
	seen := make(map[string]struct{}, len(fields))
	addresses := make([]string, 0, len(fields))
	for _, field := range fields {
		address, err := normalizeSignalAddress(field)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("signal probe server list is empty")
	}
	return addresses, nil
}

func normalizeSignalAddress(value string) (string, error) {
	address := strings.TrimSpace(value)
	hasScheme := strings.Contains(address, "://")
	parsedAddress := address
	if !hasScheme {
		parsedAddress = "ws://" + address
	}
	parsed, err := url.Parse(parsedAddress)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("signal probe server must use the IP:port/path format: %q", value)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("signal probe server scheme must be ws or wss: %q", value)
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("signal probe server must use an IPv4 address: %q", value)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("signal probe server port is outside 1-65535: %q", value)
	}
	parsed.Host = net.JoinHostPort(ip.String(), strconv.Itoa(port))
	if parsed.Path != "" {
		parsed.Path = normalizeSignalPath(parsed.Path)
	}
	parsed.RawPath = ""
	result := parsed.String()
	if !hasScheme {
		result = strings.TrimPrefix(result, "ws://")
	}
	return result, nil
}

func ResolveBindIPv4(iface, signalAddress string) (ip string, interfaceName string, err error) {
	target, err := SignalHostPort(signalAddress)
	if err != nil {
		return "", "", err
	}
	if iface != "" {
		ip, err = interfaceIPv4(iface)
		return ip, iface, err
	}

	conn, err := net.DialTimeout("tcp4", target, websocketTimeout)
	if err != nil {
		return "", "", err
	}
	host, _, splitErr := net.SplitHostPort(conn.LocalAddr().String())
	_ = conn.Close()
	if splitErr != nil {
		return "", "", splitErr
	}
	interfaceName = diag.InterfaceForIPv4(host)
	if isTunnelInterface(interfaceName) {
		return "", "", fmt.Errorf("automatic routing selected tunnel interface %s (%s); use -iface to choose the physical interface of the network under test", interfaceName, host)
	}
	return host, interfaceName, nil
}

func SignalHostPort(address string) (string, error) {
	parsedAddress := address
	if !strings.Contains(parsedAddress, "://") {
		parsedAddress = "ws://" + parsedAddress
	}
	u, err := url.Parse(parsedAddress)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid signal address %q", address)
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "ws":
			port = "80"
		case "wss":
			port = "443"
		default:
			return "", fmt.Errorf("signal address must use ws or wss: %q", address)
		}
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

func EchoPublicAddress(ctx context.Context, signalAddress, bindIP string) (string, error) {
	wsURL, err := websocketURL(signalAddress, "/echo", 0)
	if err != nil {
		return "", err
	}
	conn, transport, err := dialWebSocket(ctx, wsURL, net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return "", err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	defer transport.CloseIdleConnections()
	return readPublicAddress(ctx, conn)
}

func OpenStableEndpoint(ctx context.Context, signalAddress, bindIP string, probePorts []int) (*StableEndpoint, error) {
	targets := make([]stableProbeTarget, 0, len(probePorts))
	for _, port := range probePorts {
		targets = append(targets, stableProbeTarget{address: signalAddress, overridePort: port})
	}
	return openStableEndpoint(ctx, bindIP, targets)
}

// OpenStableEndpointSignals verifies one local endpoint against multiple
// Signal IP:port/path targets. Multiple public IPs distinguish EIM from ADM.
func OpenStableEndpointSignals(ctx context.Context, signalAddresses []string, bindIP string) (*StableEndpoint, error) {
	targets := make([]stableProbeTarget, 0, len(signalAddresses))
	for _, address := range signalAddresses {
		targets = append(targets, stableProbeTarget{address: address})
	}
	return openStableEndpoint(ctx, bindIP, targets)
}

type stableProbeTarget struct {
	address      string
	overridePort int
}

func openStableEndpoint(ctx context.Context, bindIP string, targets []stableProbeTarget) (*StableEndpoint, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("signal probe server list is empty")
	}
	localAddr, err := allocateLocalAddress(bindIP)
	if err != nil {
		return nil, err
	}
	endpoint := &StableEndpoint{LocalAddr: localAddr}
	for _, target := range targets {
		wsURL, err := websocketURL(target.address, "/nat", target.overridePort)
		if err != nil {
			endpoint.Close()
			return nil, err
		}
		targetAddress, err := SignalHostPort(wsURL)
		if err != nil {
			endpoint.Close()
			return nil, err
		}
		conn, transport, err := dialWebSocket(ctx, wsURL, localAddr)
		if err != nil {
			endpoint.Close()
			return nil, fmt.Errorf("connect to NAT probe server %s: %w", targetAddress, err)
		}
		publicAddr, err := readPublicAddress(ctx, conn)
		if err != nil {
			_ = conn.Close(websocket.StatusInternalError, "read failed")
			transport.CloseIdleConnections()
			endpoint.Close()
			return nil, fmt.Errorf("read from NAT probe server %s: %w", targetAddress, err)
		}
		endpoint.holds = append(endpoint.holds, heldWebSocket{conn: conn, transport: transport})
		endpoint.Observations = append(endpoint.Observations, MappingObservation{
			TargetAddr: targetAddress,
			PublicAddr: publicAddr,
		})
		if endpoint.PublicAddr == "" {
			endpoint.PublicAddr = publicAddr
			continue
		}
		if endpoint.PublicAddr != publicAddr {
			endpoint.Close()
			return nil, fmt.Errorf("server mapping changes with the destination port: %s vs %s", endpoint.PublicAddr, publicAddr)
		}
	}
	return endpoint, nil
}

func PublicIP(address string) (string, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	if net.ParseIP(host) == nil {
		return "", fmt.Errorf("invalid public address host: %q", host)
	}
	return host, nil
}

func allocateLocalAddress(ip string) (string, error) {
	listener, err := (&net.ListenConfig{Control: reuseControl}).Listen(context.Background(), "tcp4", net.JoinHostPort(ip, "0"))
	if err != nil {
		return "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return "", err
	}
	return net.JoinHostPort(ip, strconv.Itoa(port)), nil
}

func dialWebSocket(ctx context.Context, wsURL, localAddr string) (*websocket.Conn, *http.Transport, error) {
	dialer := &net.Dialer{
		Control:   reuseControl,
		LocalAddr: mustTCPAddr(localAddr),
		Timeout:   websocketTimeout,
	}
	transport := &http.Transport{
		Proxy:             nil,
		DialContext:       dialer.DialContext,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
	}
	client := &http.Client{Transport: transport}
	conn, response, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		transport.CloseIdleConnections()
		return nil, nil, err
	}
	return conn, transport, nil
}

func readPublicAddress(parent context.Context, conn *websocket.Conn) (string, error) {
	ctx, cancel := context.WithTimeout(parent, websocketTimeout)
	defer cancel()
	messageType, data, err := conn.Read(ctx)
	if err != nil {
		return "", err
	}
	if messageType != websocket.MessageText {
		return "", fmt.Errorf("unexpected address echo message type %v", messageType)
	}
	var response signalproto.ProbeResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return "", fmt.Errorf("invalid address echo json: %w", err)
	}
	if response.Version != signalproto.Version {
		return "", fmt.Errorf("invalid address echo protocol version: %d", response.Version)
	}
	address := strings.TrimSpace(response.Endpoint)
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil {
		return "", fmt.Errorf("invalid address echo payload: %q", address)
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 || value > 65535 {
		return "", fmt.Errorf("invalid address echo port: %q", port)
	}
	return address, nil
}

func websocketURL(address, fallbackPath string, overridePort int) (string, error) {
	if !strings.Contains(address, "://") {
		address = "ws://" + address
	}
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid signal address %q", address)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("signal address must use ws or wss: %q", address)
	}
	if overridePort > 0 {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(overridePort))
	} else if u.Port() == "" {
		port := "80"
		if u.Scheme == "wss" {
			port = "443"
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	if u.Path == "" {
		u.Path = fallbackPath
	}
	u.Path = normalizeSignalPath(u.Path)
	u.RawPath = ""
	query := u.Query()
	query.Set(signalproto.CommandKey, strings.TrimPrefix(fallbackPath, "/"))
	query.Set("room", "birthday-prototype")
	query.Set("session", "probe")
	u.RawQuery = query.Encode()
	u.Fragment = ""
	return u.String(), nil
}

func normalizeSignalPath(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "/"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return value
}

func mustTCPAddr(value string) *net.TCPAddr {
	address, err := net.ResolveTCPAddr("tcp4", value)
	if err != nil {
		panic(err)
	}
	return address
}

// interfaceIPv4 returns the IPv4 address of the named interface.
//
// It uses netiface.IPv4ByName, which prefers ioctl(SIOCGIFADDR) on
// Linux/Android and avoids netlink, so it works under the SELinux policy
// applied to non-root Android processes.
func interfaceIPv4(name string) (string, error) {
	addr, err := netiface.IPv4ByName(name)
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

func isTunnelInterface(name string) bool {
	name = strings.ToLower(name)
	return strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "tap")
}
