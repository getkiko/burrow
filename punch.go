package burrow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/getkiko/burrow/internal/netiface"
)

// This file implements the zero-prefix connection path for direct and
// predictive. The birthday ack selection prefix is consumed entirely by
// selection.go before delivery, so the application always reads from its own
// first byte.
//
// Session ownership is decided out of band: each attempt has its own local
// port, so sessions are separated by the kernel, and inbound connections are
// filtered by the peer address learned from signaling, which rules out
// unrelated sources (scanners, stale connections) without touching the data.

// role is this side's role in an attempt. It is used for logging and
// candidate management only; nothing is written on the connection.
type role int

const (
	roleListener role = iota
	roleDialer
)

func (r role) String() string {
	if r == roleListener {
		return "listener"
	}
	return "dialer"
}

// punchResult is the outcome of a successful punch.
type punchResult struct {
	conn    net.Conn // established TCP candidate; the upper layer verifies it works
	peer    string   // peer public TCP address
	passive bool     // accepted (true) or dialed (false)
}

func mustTCPAddr(addr string) *net.TCPAddr {
	a, _ := net.ResolveTCPAddr(tcpNetworkForHostPort(addr), addr)
	return a
}

func tcpNetworkForHostPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		if ip, parseErr := netip.ParseAddr(host); parseErr == nil && ip.Unmap().Is6() {
			return "tcp6"
		}
	}
	return "tcp4"
}

// bindIP picks the local IP that all punching sockets bind to.
//
// Binding 0.0.0.0 does not work: with virtual interfaces such as Docker
// bridges present, the kernel may pick an unroutable source address and send
// SYNs out of the wrong interface. So the named physical interface is used,
// and only without one does it fall back to the kernel's route to target.
func bindIP(ctx context.Context, iface, target, tcpNetwork string, control func(network, address string, c syscall.RawConn) error, resolveIP ResolveIPFunc, interfaceAddr func(string) string, debugLog DebugLogFunc) (string, error) {
	if iface != "" {
		// Prefer the address supplied by the caller: it comes from a platform
		// API without netlink and skips the ioctl and interface queries in
		// ifaceIP entirely.
		if interfaceAddr != nil {
			if ip := interfaceAddr(iface); ip != "" {
				debugLogf(debugLog, "bindIP: interface %q address from caller => %s", iface, ip)
				return ip, nil
			}
		}
		// For IPv4, ifaceIP tries internal/netiface (ioctl) first, which needs
		// no netlink on Linux and Android, so this usually succeeds without
		// reaching the isPermissionError branch below.
		ip, err := ifaceIP(iface, tcpNetwork, debugLog)
		if err == nil {
			return ip, nil
		}
		// The ioctl path did not yield an address either (non-Linux platform,
		// ioctl failure, or net.InterfaceByName/Addrs was also denied). On
		// restricted platforms such as Android, non-root processes are denied
		// netlink route table reads ("netlinkrib: permission denied"). That
		// does not make the interface unusable: sockets still bind to it via
		// SO_BINDTODEVICE/IP_BOUND_IF in Control, only its address list is
		// unavailable. Fall back to kernel routing for these errors, or
		// punching cannot work on restricted platforms at all.
		if isPermissionError(err) {
			debugLogf(debugLog, "bindIP: listing addresses of interface %q denied (%v); falling back to "+
				"kernel routing, sockets are still bound to the interface via Control", iface, err)
		} else {
			// Misspelled name, missing interface, or no address in the selected
			// family. Falling back to kernel routing is exactly the path that
			// makes punching fail, and continuing silently would only surface
			// as an unexplained punch timeout later, so fail now.
			return "", fmt.Errorf("interface %q unavailable: %w (check bind_interface)", iface, err)
		}
	}
	hostPort, err := signalHostPort(target)
	if err != nil {
		return "", err
	}
	dialer := net.Dialer{
		Timeout: defaultSignalTimeout,
		Control: socketControl(control),
	}
	c, err := dialSignalTCP(ctx, &dialer, hostPort, resolveIP, tcpNetwork)
	if err != nil {
		return "", fmt.Errorf("detect outbound IP: %w", err)
	}
	defer c.Close()
	host, _, err := net.SplitHostPort(c.LocalAddr().String())
	if err != nil {
		return "", err
	}
	return host, nil
}

// isPermissionError reports whether err is a permission error.
//
// On Android, net.InterfaceByName and Interface.Addrs in a non-root process
// fail with EPERM/EACCES wrapped in os.SyscallError{Syscall: "netlinkrib"}
// because SELinux denies netlink route table reads. That is different from a
// missing interface and must be handled separately.
func isPermissionError(err error) bool {
	return errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES)
}

// ifaceIP returns the first global address on the named interface that
// matches tcpNetwork.
//
// For IPv4 it tries netiface.IPv4ByName first, which uses ioctl
// (SIOCGIFADDR) on Linux and Android and so avoids the SELinux netlink
// restriction described at isPermissionError. That spares bindIP the lossy
// fallback to kernel routing, whose source address may not belong to the
// requested interface.
//
// If netiface does not support the platform or the ioctl fails, it falls back
// to net.InterfaceByName/Addrs; bindIP's isPermissionError check still covers
// failures on that path.
func ifaceIP(name, tcpNetwork string, debugLog DebugLogFunc) (string, error) {
	if tcpNetwork == "tcp4" {
		if addr, err := netiface.IPv4ByName(name); err == nil {
			debugLogf(debugLog, "ifaceIP(%q, tcp4): resolved by netiface (ioctl) => %s", name, addr)
			return addr.String(), nil
		}
	}

	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return "", err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", err
	}
	debugLogf(debugLog, "ifaceIP(%q, %s): index=%d addrs=%v", name, tcpNetwork, ifi.Index, addrs)
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			continue
		}
		if tcpNetwork == "tcp4" {
			if v4 := ip.To4(); v4 != nil {
				return v4.String(), nil
			}
		} else if ip.To4() == nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("interface %s has no usable %s address", name, tcpNetwork)
}

// allocLocalPort binds and releases a port on bindAddr and returns the local
// address. With SO_REUSEPORT that port can then be used for address
// reflection, listening and dialing at the same time.
func allocLocalPort(bindAddr, tcpNetwork string, control func(network, address string, c syscall.RawConn) error) (string, error) {
	lc := &net.ListenConfig{Control: socketControl(control)}
	ln, err := lc.Listen(context.Background(), tcpNetwork, net.JoinHostPort(bindAddr, "0"))
	if err != nil {
		return "", fmt.Errorf("allocate local port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return net.JoinHostPort(bindAddr, strconv.Itoa(port)), nil
}

// puncher owns a bound local port and listens on it from the moment it is
// created.
//
// Listening does not require knowing the peer, but the peer sends SYNs as
// soon as it learns our address. If listening waited for the peer address,
// SYNs arriving in that gap would be answered with RST. Listening early closes
// the gap, so the peer's SYNs are caught even if the pairing notification is
// delayed or lost; signaling reconnects or delays only affect pairing timing.
//
// Since the peer is unknown while listening early, inbound connections are
// queued and claimed by address once punch learns the peer. Nothing is ever
// written on the connection.
type puncher struct {
	localAddr string
	publicTCP string // this side's public TCP address
	self      role
	session   string
	opt       punchOptions
	network   string

	ln     net.Listener
	accCh  chan net.Conn // accepted connections waiting to be claimed by address
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (p *puncher) tcpNetwork() string {
	if p.network != "" {
		return p.network
	}
	return tcpNetworkForHostPort(p.localAddr)
}

// newPuncher starts listening on localAddr immediately. publicTCP is the
// public TCP address of that port as reflected by the signal server. The
// caller must call Close afterwards; connections already taken are not
// affected.
func newPuncher(ctx context.Context, localAddr, publicTCP string, self role, session string, opt punchOptions) (*puncher, error) {
	lc := &net.ListenConfig{Control: socketControl(opt.control)}
	network := tcpNetworkForHostPort(localAddr)
	ln, err := lc.Listen(ctx, network, localAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", localAddr, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	p := &puncher{
		localAddr: localAddr,
		publicTCP: publicTCP,
		self:      self,
		session:   session,
		opt:       opt,
		network:   network,
		ln:        ln,
		accCh:     make(chan net.Conn, accChCapacity),
		cancel:    cancel,
	}

	// Close the socket to unblock Accept in acceptLoop. This goroutine must be
	// tracked by p.wg like acceptLoop, or Close's wg.Wait would not wait for
	// it and leak detection would flag it.
	p.wg.Go(func() {
		<-ctx.Done()
		_ = p.ln.Close()
	})
	p.wg.Go(func() { p.acceptLoop(ctx) })
	return p, nil
}

// acceptLoop accepts inbound connections and queues them on accCh for punch
// to claim by peer address.
//
// It never reads or writes: connections must reach the caller untouched.
func (p *puncher) acceptLoop(ctx context.Context) {
	for ctx.Err() == nil {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		configureTCPKeepAlive(c)
		select {
		case p.accCh <- c:
		default:
			// The queue is full (usually a winner already exists); close
			// the extra connection of unknown origin.
			debugLogf(p.opt.debugLog, "puncher[%s %s] dropping extra inbound connection (%s<-%s)",
				p.self, shortSession(p.session), c.LocalAddr(), c.RemoteAddr())
			_ = c.Close()
		}
	}
}

func configureTCPKeepAlive(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(tcpKeepAlivePeriod)
	}
}

func newTCPDialer(localAddr string, control func(network, address string, c syscall.RawConn) error) *net.Dialer {
	return &net.Dialer{
		Control:   socketControl(control),
		LocalAddr: mustTCPAddr(localAddr),
		KeepAlive: tcpKeepAlivePeriod,
	}
}

// punch supplies the peer address, starts the dialing direction and blocks
// until a connection is established or the attempt times out. The passive
// direction has been running since newPuncher; its result joins the race.
//
// Dials to a single target are sequential: one (local port, peer address)
// 4-tuple allows only one connection in flight, and parallel dials to the
// same target get EADDRINUSE, so frequent retries cover the simultaneity
// window instead.
func (p *puncher) punch(ctx context.Context, peer string) (*punchResult, error) {
	var got *punchResult
	err := p.punchCandidates(ctx, peer, 1, 0, func(res *punchResult) error {
		got = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	if got == nil {
		// punchCandidates returns nil only after delivering a candidate, so
		// this is unreachable; it guards against returning (nil, nil) after
		// future changes.
		return nil, fmt.Errorf("punch to %s failed (no candidate): %w", peer, ErrPunchFailed)
	}
	return got, nil
}

// claimPassive claims an inbound connection by peer address. The check is out
// of band and never reads the connection, so application data is untouched.
// Connections from other sources (scanners, stale peers) are closed.
func (p *puncher) claimPassive(c net.Conn, peer string) bool {
	if sameHost(c.RemoteAddr().String(), peer) {
		return true
	}
	debugLogf(p.opt.debugLog, "puncher[%s %s] dropping connection from unexpected source: %s (want %s)",
		p.self, shortSession(p.session), c.RemoteAddr(), peer)
	_ = c.Close()
	return false
}

// startDial starts the dialing direction: single-port retries or a port
// window scan.
func (p *puncher) startDial(ctx context.Context, peer string, wg *sync.WaitGroup, dialCh chan<- net.Conn, dialErr *atomic.Pointer[error]) {
	if p.opt.portScanRange > 0 {
		wg.Go(func() { p.scanDial(ctx, peer, dialCh, dialErr) })
		return
	}
	wg.Go(func() { p.singleDial(ctx, peer, dialCh, dialErr) })
}

// drainCandidate picks up an already ready candidate without blocking.
//
// The timeout and a just-established connection can become ready together,
// and select chooses randomly among ready cases, so the timeout branch must
// check the buffers once more or it would discard a working connection.
func (p *puncher) drainCandidate(peer string, dialCh <-chan net.Conn) *punchResult {
	for {
		select {
		case c := <-p.accCh:
			if !p.claimPassive(c, peer) {
				continue
			}
			return &punchResult{conn: c, peer: peer, passive: true}
		default:
		}
		break
	}
	select {
	case c := <-dialCh:
		return &punchResult{conn: c, peer: peer}
	default:
		return nil
	}
}

// punchFailure builds a diagnosable error from the last dial error.
func punchFailure(peer string, dialErr *atomic.Pointer[error]) error {
	if lastErr := dialErr.Load(); lastErr != nil {
		return fmt.Errorf("punch to %s failed (last error: %v): %w",
			peer, *lastErr, ErrPunchFailed)
	}
	return fmt.Errorf("punch to %s timed out (no connection from peer): %w", peer, ErrPunchFailed)
}

// punchCandidates runs one punching round and passes candidates to yield.
// firstCandidateGrace is how long to wait for the other direction's candidate
// after the first one. Candidates are never read or written.
func (p *puncher) punchCandidates(ctx context.Context, peer string, maxCandidates int, firstCandidateGrace time.Duration, yield func(*punchResult) error) error {
	if maxCandidates < 1 {
		maxCandidates = 1
	}
	attemptCtx, cancel := context.WithTimeout(ctx, p.opt.punchTimeout)
	defer cancel()
	dialCtx, stopDial := context.WithCancel(attemptCtx)
	defer stopDial()

	debugLogf(p.opt.debugLog, "puncher[%s %s] punching: local %s (public %s) -> peer %s (both directions)",
		p.self, shortSession(p.session), p.localAddr, p.publicTCP, peer)

	dialCh := make(chan net.Conn, 1)
	var dialErr atomic.Pointer[error]
	var wg sync.WaitGroup
	p.startDial(dialCtx, peer, &wg, dialCh, &dialErr)
	defer func() {
		stopDial()
		cancel()
		wg.Wait()
		for {
			select {
			case c := <-dialCh:
				_ = c.Close()
			default:
				return
			}
		}
	}()

	delivered := 0
	activeDelivered := false
	passiveDelivered := false
	var graceTimer *time.Timer
	var graceCh <-chan time.Time
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()

	deliver := func(res *punchResult) error {
		if err := yield(res); err != nil {
			_ = res.conn.Close()
			return err
		}
		delivered++
		if delivered == 1 && firstCandidateGrace > 0 {
			graceTimer = time.NewTimer(firstCandidateGrace)
			graceCh = graceTimer.C
		}
		return nil
	}

	for {
		select {
		case c := <-dialCh:
			if activeDelivered {
				_ = c.Close()
				continue
			}
			activeDelivered = true
			stopDial()
			if err := deliver(&punchResult{conn: c, peer: peer}); err != nil {
				return err
			}
			if delivered >= maxCandidates {
				return nil
			}
		case c := <-p.accCh:
			if !p.claimPassive(c, peer) {
				continue
			}
			if passiveDelivered {
				_ = c.Close()
				continue
			}
			passiveDelivered = true
			if err := deliver(&punchResult{conn: c, peer: peer, passive: true}); err != nil {
				return err
			}
			if delivered >= maxCandidates {
				return nil
			}
		case <-graceCh:
			if delivered > 0 {
				return nil
			}
		case <-attemptCtx.Done():
			if delivered > 0 {
				return nil
			}
			// Timeout and success can be ready together; pick up a buffered
			// candidate once more.
			if res := p.drainCandidate(peer, dialCh); res != nil {
				return deliver(res)
			}
			return punchFailure(peer, &dialErr)
		}
	}
}

// singleDial is the default dialing direction: it retries the single port the
// signal server reflected for the peer (suits cone NATs). The first success
// is sent on dialCh and it returns.
func (p *puncher) singleDial(ctx context.Context, peer string, dialCh chan<- net.Conn, dialErr *atomic.Pointer[error]) {
	for ctx.Err() == nil {
		d := newTCPDialer(p.localAddr, p.opt.control)
		dctx, dcancel := context.WithTimeout(ctx, p.opt.dialTimeout)
		c, err := d.DialContext(dctx, p.tcpNetwork(), peer)
		dcancel()
		if err == nil {
			select {
			case dialCh <- c:
			default:
				_ = c.Close()
			}
			return
		}
		recordDialErr(dialErr, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.opt.dialInterval):
		}
	}
}

// scanDial is the port prediction dialing direction, used when a scan range
// is set. It dials the window computed by scanWindow around the peer's
// reflected port in parallel to cover the unpredictable public port of a
// symmetric NAT. The first port that connects is sent on dialCh and it
// returns.
//
// Distinct target ports give distinct 4-tuples, so dials can run in parallel
// without EADDRINUSE; a semaphore bounds concurrency. After each sweep it
// pauses scanRoundInterval, since the peer's mapping may only be ready in the
// next round. Nothing is written on the connection: the first established one
// wins and the upper layer verifies it works.
func (p *puncher) scanDial(ctx context.Context, peer string, dialCh chan<- net.Conn, dialErr *atomic.Pointer[error]) {
	host, basePort, err := splitHostPortInt(peer)
	if err != nil {
		dialErr.Store(&err)
		return
	}
	startPort, endPort := scanWindow(basePort, p.opt.portScanRange)
	concurrency := p.opt.scanConcurrency
	if concurrency <= 0 {
		concurrency = scanConcurrency
	}
	debugLogf(p.opt.debugLog, "puncher[%s %s] port scan: peer %s:[%d-%d] (%d ports, concurrency %d)",
		p.self, shortSession(p.session), host, startPort, endPort,
		endPort-startPort+1, concurrency)

	// scanCtx lets the first hit cancel the remaining dials of the round at
	// once. Otherwise, even if the third of 512 targets connects, the other
	// 509 each run until scanDialTimeout, delaying the return and piling up
	// useless SYN_SENT/TIME_WAIT sockets in the kernel.
	scanCtx, scanCancel := context.WithCancel(ctx)
	defer scanCancel()

	sem := make(chan struct{}, concurrency)
	for scanCtx.Err() == nil {
		var roundWg sync.WaitGroup
		for port := startPort; port <= endPort && scanCtx.Err() == nil; port++ {
			target := net.JoinHostPort(host, strconv.Itoa(port))
			select {
			case sem <- struct{}{}:
			case <-scanCtx.Done():
				roundWg.Wait()
				return
			}
			roundWg.Go(func() {
				defer func() { <-sem }()
				d := newTCPDialer(p.localAddr, p.opt.control)
				dctx, dcancel := context.WithTimeout(scanCtx, scanDialTimeout)
				c, derr := d.DialContext(dctx, p.tcpNetwork(), target)
				dcancel()
				if derr != nil {
					recordDialErr(dialErr, derr)
					return
				}
				select {
				case dialCh <- c:
					scanCancel() // hit: stop the rest of this round
				default:
					_ = c.Close() // a winner already exists
				}
			})
		}
		roundWg.Wait()
		select {
		case <-scanCtx.Done():
			return
		case <-time.After(scanRoundInterval):
		}
	}
}

// scanWindow computes the port scan window.
//
// The window is centered on basePort rather than extending upward only.
// Symmetric NATs usually allocate increasing ports, but the port the peer
// actually punches from is allocated after the reflection, and other
// connections consuming ports in between can put the target on either side
// of basePort. A one-sided window always misses a target below basePort.
func scanWindow(basePort, scanRange int) (start, end int) {
	if scanRange <= 0 {
		return basePort, basePort
	}
	// Fix the width first, then shift the window to fit the bounds. Clamping
	// the lower bound first and compensating afterwards would silently narrow
	// the window near low ports.
	width := scanRange
	start = basePort - scanRange/2
	if start < 1 {
		start = 1
	}
	end = start + width
	if end > 65535 {
		end = 65535
		if start = end - width; start < 1 {
			start = 1
		}
	}
	return start, end
}

// recordDialErr stores a dial error worth reporting.
//
// While scanning, hundreds of parallel dials compete for one slot. Plain
// overwriting would usually leave a "connection refused" from some irrelevant
// port in the window, hiding fatal local errors such as EADDRNOTAVAIL or
// ENETUNREACH that point to a misconfigured interface or source address.
// Local errors therefore take priority and are never overwritten by ordinary
// connection errors.
func recordDialErr(slot *atomic.Pointer[error], err error) {
	if err == nil {
		return
	}
	if !isLocalDialErr(err) {
		// Ordinary error: store only if the slot is empty, never over a
		// recorded local error.
		slot.CompareAndSwap(nil, &err)
		return
	}
	slot.Store(&err)
}

// isLocalDialErr reports whether err comes from local configuration rather
// than an unreachable peer.
func isLocalDialErr(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) ||
		errors.Is(err, syscall.EADDRINUSE) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE)
}

// splitHostPortInt splits "ip:port" into host and an integer port for port
// scanning.
func splitHostPortInt(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port out of range: %d", port)
	}
	return host, port, nil
}

// Close stops listening and waits for internal goroutines. Connections
// already returned by punch are not affected.
func (p *puncher) Close() error {
	p.cancel()
	_ = p.ln.Close()
	p.wg.Wait()
	// Close any queued connections nobody claimed.
	for {
		select {
		case c := <-p.accCh:
			_ = c.Close()
		default:
			return nil
		}
	}
}

// sameHost reports whether two "ip:port" strings have the same IP.
func sameHost(a, b string) bool {
	ha, _, err1 := net.SplitHostPort(a)
	hb, _, err2 := net.SplitHostPort(b)
	if err1 != nil || err2 != nil {
		return false
	}
	ipA, parseErrA := netip.ParseAddr(ha)
	ipB, parseErrB := netip.ParseAddr(hb)
	if parseErrA != nil || parseErrB != nil {
		return false
	}
	return ipA.Unmap() == ipB.Unmap()
}
