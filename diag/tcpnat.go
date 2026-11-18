package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/netiface"
	"github.com/getkiko/burrow/internal/signalproto"
)

// DefaultProbeTimeout is the default timeout of each probe connection.
const DefaultProbeTimeout = 8 * time.Second

type observation struct {
	target     string
	publicAddr string
	publicIP   string
	publicPort int
	conn       *websocket.Conn
	transport  *http.Transport
}

// TCPNATConfig is the input of a TCP NAT probe.
type TCPNATConfig struct {
	Server     string        // public signal server address, IP:port or IP:port/path
	Ports      string        // comma-separated probe ports; empty means "9960,9962,9963"
	Iface      string        // outbound interface; empty lets the kernel route
	LocalPort  int           // fixed local TCP port; 0 allocates a new one each round
	Rounds     int           // number of probe rounds; <=0 is invalid
	Timeout    time.Duration // timeout of each connection; <=0 means DefaultProbeTimeout
	SignalPath string        // signaling path prefix; ignored if Server already has a path
	Proxy      string        // HTTP proxy (e.g. 10.0.0.200:80); empty connects directly
}

// NATMapping is the classified mapping behavior.
//
// Only two classes are distinguished. With a single destination IP one can
// only observe whether the mapping changes with the destination port, which
// is exactly the test for a strict (APDM/symmetric) NAT. Telling EIM (full
// cone) from ADM (restricted cone) would need a second destination IP, since
// both behave identically across ports of one IP. The distinction is
// deliberately not made: for hole punching what matters is strict versus
// non-strict, and both EIM and ADM can act as the listener.
type NATMapping string

const (
	// MappingUnknown means the observations failed or were insufficient.
	MappingUnknown NATMapping = "unknown"
	// MappingStable means one local port keeps the same public mapping toward
	// different ports of the same IP, i.e. a non-strict NAT (EIM or ADM). It
	// can act as the punching listener.
	MappingStable NATMapping = "stable"
	// MappingSymmetric is a strict NAT (APDM/symmetric): a new destination
	// port gets a new public port. It cannot act as the listener, because the
	// public endpoint depends on the destination while signaling must finish
	// before punching, so the listener cannot know which port it will have
	// toward a given client.
	MappingSymmetric NATMapping = "symmetric"
)

// PortAllocation describes how a symmetric NAT allocates public ports.
//
// It only matters for APDM; EIM/ADM ports are stable anyway. For APDM it
// decides whether the predictive strategy has a chance: sequential
// allocation can be predicted, random allocation cannot.
type PortAllocation string

const (
	PortAllocUnknown    PortAllocation = "unknown"
	PortAllocSequential PortAllocation = "sequential" // +1 or small increments
	PortAllocRandom     PortAllocation = "random"     // no pattern
)

// TCPNATResult is the outcome of a TCP NAT probe.
type TCPNATResult struct {
	// Detail is the full per-round text report for checking raw observations.
	Detail string

	// Mapping is the classified mapping behavior (strict or not).
	Mapping NATMapping
	// MappingReason states in one sentence which observation the verdict is
	// based on, so users can verify it instead of seeing only a label.
	MappingReason string

	// PortAlloc is meaningful only when Mapping is symmetric (APDM).
	PortAlloc PortAllocation
	// PortDrift holds the public port deltas of the same target between
	// consecutive rounds. Smaller, steadier deltas make predictive more
	// likely to succeed.
	PortDrift []int
	// CrossTargetSpread is the largest public port spread across targets
	// within one round. Predictive must infer the port toward the client from
	// the port toward the signal server; this spread is the distance to
	// cover and determines the scan window size.
	CrossTargetSpread int

	// PublicIPStable is false if observations saw different public IPs
	// (multi-egress NAT or CGNAT load balancing). No punching strategy works
	// then; a relay is required.
	PublicIPStable bool

	// CanBeListener reports whether this side can act as the punching
	// listener (server).
	CanBeListener bool
	// Advice is a recommendation that can be shown to the user directly.
	Advice string
}

// RunTCPNAT probes the TCP NAT mapping behavior.
//
// Even when it returns an error, TCPNATResult.Detail may contain the partial
// report produced so far.
func RunTCPNAT(ctx context.Context, cfg TCPNATConfig) (TCPNATResult, error) {
	var buf bytes.Buffer
	fail := func(err error) (TCPNATResult, error) {
		return TCPNATResult{Detail: buf.String()}, err
	}

	if strings.TrimSpace(cfg.Server) == "" {
		return fail(fmt.Errorf("server is required: specify the public signal server"))
	}
	if cfg.Rounds <= 0 {
		return fail(fmt.Errorf("rounds must be greater than 0"))
	}
	ports := cfg.Ports
	if strings.TrimSpace(ports) == "" {
		ports = "9960,9962,9963"
	}
	parsedPorts, err := ParsePorts(ports)
	if err != nil {
		return fail(err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	host, prefix := resolveTCPNATServer(cfg.Server, cfg.SignalPath)
	if host == "" {
		return fail(fmt.Errorf("server is missing a host name or IP"))
	}
	targets := makeTargets(host, parsedPorts)

	// No local address binding when using a proxy.
	//
	// Connections go to the proxy, which connects to the target. Probing the
	// kernel route toward the target would fail (behind a WAP access point
	// the target is not directly reachable: "cannot assign requested
	// address"), and would be meaningless anyway: the local port only
	// applies to the hop to the proxy, which uses its own port toward the
	// target.
	//
	// In proxy mode the probe therefore measures the proxy's mapping
	// behavior, not the local one. That is still useful for hole punching,
	// since the peer sees exactly the proxy's endpoint.
	var bindIP, localAddr string
	if cfg.Proxy == "" {
		var err error
		bindIP, err = selectBindIPv4(ctx, cfg.Iface, targets[0], timeout)
		if err != nil {
			return fail(fmt.Errorf("determine outbound IPv4: %w", err))
		}
		selectedInterface := InterfaceForIPv4(bindIP)
		if cfg.Iface == "" && isTunnelInterface(selectedInterface) {
			return fail(fmt.Errorf("automatic routing selected tunnel interface %s (%s); use iface to choose the physical interface of the network under test", selectedInterface, bindIP))
		}
		fmt.Fprintf(&buf, "[tcpnat] server=%s target_ports=%v interface=%s local_ip=%s\n", cfg.Server, parsedPorts, selectedInterface, bindIP)
	} else {
		fmt.Fprintf(&buf, "[tcpnat] server=%s target_ports=%v proxy=%s (measuring the proxy's mapping behavior)\n", cfg.Server, parsedPorts, cfg.Proxy)
	}
	// Accumulate raw observations per round and classify at the end: port
	// drift needs cross-round comparison, while one round only shows whether
	// the mapping changed within it.
	var (
		perRoundPorts [][]int  // [round][target] public port
		publicIPs     []string // every observed public IP, to check egress stability
	)

	var sameMappingRounds int
	for round := 1; round <= cfg.Rounds; round++ {
		// In proxy mode there is no comparable local port (it only applies
		// to the hop to the proxy); 0 means "not compared".
		port := 0
		if cfg.Proxy == "" {
			port = cfg.LocalPort
			if port == 0 {
				var portErr error
				port, portErr = allocateLocalPort(ctx, bindIP, cfg.Iface)
				if portErr != nil {
					return fail(fmt.Errorf("round %d: allocate local port: %w", round, portErr))
				}
			}
			localAddr = net.JoinHostPort(bindIP, strconv.Itoa(port))
		}
		observations, err := probeRound(ctx, targets, prefix, localAddr, cfg.Iface, cfg.Proxy, timeout)
		if err != nil {
			return fail(fmt.Errorf("round %d: probe: %w", round, err))
		}
		fmt.Fprintf(&buf, "\n[round %d] fixed local address %s\n", round, localAddr)
		for index, item := range observations {
			delta := ""
			if index > 0 {
				delta = fmt.Sprintf("  public port delta %+d", item.publicPort-observations[index-1].publicPort)
			}
			fmt.Fprintf(&buf, "  %s -> %s%s\n", item.target, item.publicAddr, delta)
		}
		if describeRound(&buf, observations, port) {
			sameMappingRounds++
		}
		// Copy out the needed values before closing the connections, so a
		// closed conn is never used by mistake.
		portsThisRound := make([]int, 0, len(observations))
		for _, item := range observations {
			portsThisRound = append(portsThisRound, item.publicPort)
			publicIPs = append(publicIPs, item.publicIP)
		}
		perRoundPorts = append(perRoundPorts, portsThisRound)
		closeObservations(observations)
	}

	result := classifyNAT(perRoundPorts, publicIPs, sameMappingRounds, cfg.Rounds, cfg.Proxy != "")
	writeVerdict(&buf, result, cfg.Rounds)
	result.Detail = buf.String()
	return result, nil
}

// classifyNAT turns per-round observations into a verdict.
//
// The single criterion: does the public port change when one local port
// connects to different ports of the same IP? Changes mean strict
// (APDM/symmetric); no change means non-strict (EIM or ADM). Telling EIM
// from ADM needs a second destination IP and is deliberately not guessed;
// both are equivalent for hole punching.
func classifyNAT(perRoundPorts [][]int, publicIPs []string, sameRounds, totalRounds int, viaProxy bool) TCPNATResult {
	result := TCPNATResult{
		Mapping:   MappingUnknown,
		PortAlloc: PortAllocUnknown,
	}
	if len(perRoundPorts) == 0 {
		result.MappingReason = "no successful observations"
		result.Advice = "The probe produced no valid results; first make sure the signal server is reachable."
		return result
	}

	// Public IP stability. A multi-egress NAT or CGNAT load balancing can
	// present different public IPs on different connections from the same
	// host. No punching strategy works then (the peer may target the wrong
	// IP), so report it first; it is worse than any mapping type.
	result.PublicIPStable = true
	for _, ip := range publicIPs[1:] {
		if ip != publicIPs[0] {
			result.PublicIPStable = false
			break
		}
	}

	// Main criterion: if the mapping changed with the destination port in
	// any round, the NAT is strict.
	//
	// "Any round" rather than "all rounds": strictness is structural. Once a
	// mapping is seen to change with the destination port, the NAT allocates
	// per destination; a later round that happens to match (a coincidental
	// port pool hit) cannot disprove it. Classifying a fluctuating NAT as
	// non-strict would lead users to configure birthday and then hit
	// intermittent failures on real devices.
	switch {
	case sameRounds == totalRounds:
		result.Mapping = MappingStable
		result.MappingReason = fmt.Sprintf(
			"all %d rounds: the same local port kept the same public mapping toward different ports of one IP", totalRounds)
	case sameRounds == 0:
		result.Mapping = MappingSymmetric
		result.MappingReason = fmt.Sprintf(
			"all %d rounds: the same local port got a different public port for each destination port", totalRounds)
	default:
		// Fluctuating: treat as strict (see above), but report the
		// inconsistent observations honestly.
		result.Mapping = MappingSymmetric
		result.MappingReason = fmt.Sprintf(
			"%d/%d rounds kept one mapping and the rest changed with the destination port; any change is treated as strict",
			sameRounds, totalRounds)
	}

	result.CrossTargetSpread = crossTargetSpread(perRoundPorts)
	result.PortDrift = portDrift(perRoundPorts)
	result.PortAlloc = classifyPortAlloc(result.PortDrift)

	// Listener feasibility.
	result.CanBeListener = result.Mapping == MappingStable && result.PublicIPStable
	result.Advice = buildAdvice(result, viaProxy)
	return result
}

// crossTargetSpread returns the maximum, over all rounds, of the public port
// spread across targets within a round.
//
// It determines the predictive scan window: the client only sees the port
// toward the signal server but must guess the port toward itself, and this
// spread is the distance between the two.
func crossTargetSpread(perRoundPorts [][]int) int {
	spread := 0
	for _, ports := range perRoundPorts {
		if len(ports) < 2 {
			continue
		}
		lo, hi := ports[0], ports[0]
		for _, p := range ports[1:] {
			lo = min(lo, p)
			hi = max(hi, p)
		}
		spread = max(spread, hi-lo)
	}
	return spread
}

// portDrift returns the public port deltas of each target between
// consecutive rounds.
//
// A sequentially allocating NAT yields small, steady deltas (mostly +1),
// which makes predictive likely to hit; erratic deltas mean the port pool is
// shared with other traffic, which disturbs prediction.
func portDrift(perRoundPorts [][]int) []int {
	if len(perRoundPorts) < 2 {
		return nil
	}
	var drift []int
	for round := 1; round < len(perRoundPorts); round++ {
		prev, cur := perRoundPorts[round-1], perRoundPorts[round]
		for index := range cur {
			if index >= len(prev) {
				break
			}
			drift = append(drift, cur[index]-prev[index])
		}
	}
	return drift
}

// classifyPortAlloc decides from the delta samples whether port allocation
// is sequential.
//
// Criterion: most deltas are positive and at most seqDriftLimit. A few large
// jumps are tolerated (other traffic sharing the port pool causes them); as
// long as small increments dominate, predictive still has a chance.
func classifyPortAlloc(drift []int) PortAllocation {
	if len(drift) == 0 {
		return PortAllocUnknown
	}
	const seqDriftLimit = 64
	small := 0
	for _, d := range drift {
		if d > 0 && d <= seqDriftLimit {
			small++
		}
	}
	if small*2 > len(drift) { // more than half are small positive deltas
		return PortAllocSequential
	}
	return PortAllocRandom
}

// buildAdvice returns a recommendation that can be shown to the user.
func buildAdvice(r TCPNATResult, viaProxy bool) string {
	if !r.PublicIPStable {
		return "The public IP differed between observations (multi-egress NAT or carrier CGNAT load balancing). " +
			"The peer cannot know which IP to connect to, so no punching strategy works; use a relay."
	}
	if r.Mapping == MappingStable {
		advice := "Non-strict NAT: this side can act as the server (listener); the birthday strategy applies."
		if viaProxy {
			advice += " Note: this probe went through a proxy, so the verdict reflects the proxy's mapping behavior, not this host's."
		}
		return advice
	}

	// Strict: explain why this side cannot be the server and what options remain.
	advice := "Strict (symmetric) NAT: this side CANNOT act as the server (listener). " +
		"The public port depends on the destination, while signaling must finish before punching, " +
		"so when announcing its address it cannot know which port it will have toward the client. birthday refuses to start.\n" +
		"  Recommended: run this side as the client (dialer). The client connects outbound and is unaffected by the mapping type."

	if r.PortAlloc == PortAllocSequential {
		window := suggestScanRange(r.CrossTargetSpread)
		advice += fmt.Sprintf("\n  If it MUST be the server: ports are allocated sequentially, so predictive has some chance; "+
			"set port_scan_range >= %d on the client (based on a cross-target port spread of %d). "+
			"The success rate depends on how busy the network is and must be tested.", window, r.CrossTargetSpread)
	} else {
		advice += "\n  Port allocation shows no clear pattern, so predictive is unlikely to hit; if both sides are strict, hole punching cannot work and a relay is needed."
	}
	return advice
}

// suggestScanRange recommends a scan window from the cross-target port
// spread.
//
// The window is centered (scanWindow covers base +/- range/2), so covering
// spread needs range >= 2*spread; doubling that leaves margin for drift, and
// the result is rounded up to a power of two. The floor of 512 matches the
// upstream default; the cap of 8192 avoids triggering firewall scan alarms.
func suggestScanRange(spread int) int {
	needed := spread * 4
	window := 512
	for window < needed && window < 8192 {
		window *= 2
	}
	return window
}

// writeVerdict writes the human-readable verdict section.
func writeVerdict(output io.Writer, r TCPNATResult, rounds int) {
	fmt.Fprintln(output)
	fmt.Fprintln(output, "======== Verdict ========")

	label := map[NATMapping]string{
		MappingStable:    "non-strict (EIM/ADM, i.e. full cone or restricted cone)",
		MappingSymmetric: "strict (symmetric / APDM)",
		MappingUnknown:   "undetermined",
	}[r.Mapping]
	fmt.Fprintf(output, "NAT type: %s\n", label)
	fmt.Fprintf(output, "  Basis: %s\n", r.MappingReason)

	if r.PublicIPStable {
		fmt.Fprintf(output, "  Public IP: consistent across %d rounds\n", rounds)
	} else {
		fmt.Fprintln(output, "  Public IP: WARNING inconsistent across observations (multi-egress NAT / CGNAT load balancing)")
	}

	fmt.Fprintln(output)
	fmt.Fprintln(output, "Hole punching feasibility:")
	if r.CanBeListener {
		fmt.Fprintln(output, "  As server (listener): yes")
	} else {
		fmt.Fprintln(output, "  As server (listener): no")
	}
	// The client side is unaffected by the mapping type: it connects
	// outbound, which works behind any NAT.
	fmt.Fprintln(output, "  As client (dialer):   yes (unaffected by the mapping type)")

	if r.Mapping == MappingSymmetric {
		fmt.Fprintln(output)
		fmt.Fprintln(output, "Predictive reference:")
		fmt.Fprintf(output, "  Cross-target port spread: %d\n", r.CrossTargetSpread)
		if len(r.PortDrift) > 0 {
			fmt.Fprintf(output, "  Cross-round port drift:   %v\n", r.PortDrift)
		}
		allocLabel := map[PortAllocation]string{
			PortAllocSequential: "sequential (may be predictable)",
			PortAllocRandom:     "no clear pattern (hard to predict)",
			PortAllocUnknown:    "not enough samples",
		}[r.PortAlloc]
		fmt.Fprintf(output, "  Port allocation:          %s\n", allocLabel)
	}

	fmt.Fprintln(output)
	fmt.Fprintf(output, "Advice: %s\n", r.Advice)
}

// ParsePorts parses a comma-separated port list. After removing duplicates
// at least two distinct ports are required.
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
	if len(ports) < 2 {
		return nil, fmt.Errorf("at least two distinct probe ports are required")
	}
	return ports, nil
}

// splitServerPrefix splits -server into a host and a path prefix.
// It accepts forms such as "1.2.3.4", "1.2.3.4/abc" and "ws://1.2.3.4/abc".
// The returned prefix matches the server's normalizePrefix: a leading slash
// is added, trailing slashes are removed, and empty becomes "/".
func splitServerPrefix(value string) (string, string) {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, "://"); index >= 0 {
		value = value[index+3:]
	}
	host, path, found := strings.Cut(value, "/")
	host = strings.TrimSpace(host)
	if !found {
		return host, "/"
	}
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return host, "/"
	}
	return host, "/" + path
}

func resolveTCPNATServer(server, signalPath string) (string, string) {
	host, embeddedPath := splitServerPrefix(server)
	if strings.TrimSpace(signalPath) == "" {
		return host, embeddedPath
	}
	return host, normalizeSignalPath(signalPath)
}

func makeTargets(server string, ports []int) []string {
	targets := make([]string, 0, len(ports))
	for _, port := range ports {
		targets = append(targets, net.JoinHostPort(server, strconv.Itoa(port)))
	}
	return targets
}

// selectBindIPv4 looks up the IPv4 address of the named interface (through
// netiface, which avoids Android's netlink restriction). Without an
// interface it asks the kernel's routing for the outbound address.
func selectBindIPv4(ctx context.Context, iface, target string, timeout time.Duration) (string, error) {
	if iface != "" {
		addr, err := netiface.IPv4ByName(iface)
		if err != nil {
			return "", err
		}
		return addr.String(), nil
	}

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp4", target)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	return host, err
}

// bindControlFor combines reuseControl with binding to the named interface.
//
// Binding the source IP alone is not enough: when a system-wide tun proxy
// hijacks the default route, the kernel still hands packets to the tun
// device and the probe would measure the proxy egress instead of this host.
// A wrong verdict misleads troubleshooting, which is worse than none. With an
// empty iface or no platform support it is plain reuseControl.
func bindControlFor(iface string) func(network, address string, conn syscall.RawConn) error {
	bind := netiface.BindControl(iface)
	if bind == nil {
		return reuseControl
	}
	return func(network, address string, conn syscall.RawConn) error {
		if err := reuseControl(network, address, conn); err != nil {
			return err
		}
		return bind(network, address, conn)
	}
}

func isTunnelInterface(name string) bool {
	name = strings.ToLower(name)
	return strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "tap")
}

func allocateLocalPort(ctx context.Context, ip, iface string) (int, error) {
	listener, err := (&net.ListenConfig{Control: bindControlFor(iface)}).Listen(ctx, "tcp4", net.JoinHostPort(ip, "0"))
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func probeRound(ctx context.Context, targets []string, prefix, localAddr, iface, proxy string, timeout time.Duration) ([]observation, error) {
	observations := make([]observation, 0, len(targets))
	for _, target := range targets {
		item, err := probeOne(ctx, target, prefix, localAddr, iface, proxy, timeout)
		if err != nil {
			closeObservations(observations)
			return nil, fmt.Errorf("%s: %w", target, err)
		}
		observations = append(observations, item)
	}
	return observations, nil
}

func probeOne(parent context.Context, target, prefix, localAddr, iface, proxy string, timeout time.Duration) (observation, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	dialer := &net.Dialer{
		Control:   bindControlFor(iface),
		LocalAddr: mustTCPAddr(localAddr),
		Timeout:   timeout,
	}
	// With a proxy, use our own CONNECT dialer, not Transport.Proxy: the
	// standard library only sends CONNECT for https, while http/ws requests
	// become an absolute-URI GET that the proxy handles as plain HTTP, so the
	// WebSocket Upgrade never completes and the read blocks until timeout.
	dialContext := dialer.DialContext
	if addr := normalizeProxyAddr(proxy); addr != "" {
		dialContext = connectDialer(dialer, addr)
	}
	transport := &http.Transport{
		Proxy:             nil,
		DialContext:       dialContext,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
	}
	client := &http.Client{Transport: transport}
	query := url.Values{
		signalproto.CommandKey: []string{signalproto.CommandNAT},
		"room":                 []string{"tcpnat"},
		"session":              []string{"probe"},
	}
	endpoint := "ws://" + target + prefix + "?" + query.Encode()
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		transport.CloseIdleConnections()
		return observation{}, err
	}
	messageType, data, err := conn.Read(ctx)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "read failed")
		transport.CloseIdleConnections()
		return observation{}, err
	}
	if messageType != websocket.MessageText {
		_ = conn.Close(websocket.StatusUnsupportedData, "text required")
		transport.CloseIdleConnections()
		return observation{}, fmt.Errorf("unexpected echo message type %v", messageType)
	}
	var probe signalproto.ProbeResponse
	if err := json.Unmarshal(data, &probe); err != nil {
		_ = conn.Close(websocket.StatusInvalidFramePayloadData, "invalid response")
		transport.CloseIdleConnections()
		return observation{}, fmt.Errorf("decode signal echo: %w", err)
	}
	if probe.Version != signalproto.Version {
		_ = conn.Close(websocket.StatusInvalidFramePayloadData, "invalid version")
		transport.CloseIdleConnections()
		return observation{}, fmt.Errorf("invalid signal echo version %d", probe.Version)
	}
	publicAddr := strings.TrimSpace(probe.Endpoint)
	publicIP, publicPortValue, err := net.SplitHostPort(publicAddr)
	if err != nil {
		_ = conn.Close(websocket.StatusInvalidFramePayloadData, "invalid address")
		transport.CloseIdleConnections()
		return observation{}, fmt.Errorf("parse echoed address %q: %w", publicAddr, err)
	}
	publicPort, err := strconv.Atoi(publicPortValue)
	if err != nil {
		_ = conn.Close(websocket.StatusInvalidFramePayloadData, "invalid port")
		transport.CloseIdleConnections()
		return observation{}, fmt.Errorf("parse public port %q: %w", publicPortValue, err)
	}
	return observation{
		target:     target,
		publicAddr: publicAddr,
		publicIP:   publicIP,
		publicPort: publicPort,
		conn:       conn,
		transport:  transport,
	}, nil
}

func describeRound(output io.Writer, observations []observation, localPort int) bool {
	first := observations[0]
	sameEndpoint := true
	for _, item := range observations[1:] {
		if item.publicIP != first.publicIP || item.publicPort != first.publicPort {
			sameEndpoint = false
			break
		}
	}
	if sameEndpoint {
		switch {
		case localPort == 0:
			// Proxy mode: no comparable local port, report only the mapping.
			fmt.Fprintf(output, "  Result: same public mapping %d\n", first.publicPort)
		case first.publicPort == localPort:
			fmt.Fprintln(output, "  Result: same public mapping, port preserved")
		default:
			fmt.Fprintf(output, "  Result: same public mapping, local port %d translated to %d\n", localPort, first.publicPort)
		}
		return true
	}
	fmt.Fprintln(output, "  Result: mapping changes with the destination port")
	return false
}

func closeObservations(observations []observation) {
	for _, item := range observations {
		_ = item.conn.Close(websocket.StatusNormalClosure, "round complete")
		item.transport.CloseIdleConnections()
	}
}

func mustTCPAddr(value string) *net.TCPAddr {
	address, err := net.ResolveTCPAddr("tcp4", value)
	if err != nil {
		panic(err)
	}
	return address
}

// normalizeSignalPath normalizes a signaling path prefix: it adds a leading
// slash, removes trailing slashes, and turns empty into "/".
//
// The same logic exists in cmd/burrow/signal_path.go and
// internal/birthdaylab/network.go; each serves a different package boundary
// (CLI flags, the birthday experiment, diag probes) and is kept separate.
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
