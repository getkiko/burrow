package burrow

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// This file sends and receives probe packets on UDP candidate paths.
//
// Socket-phase invariant: a socket must finish STUN before candidate
// packets are sent on it, and must not run STUN again afterwards. Each
// socket is modeled as an explicit two-phase state machine
// (udpSocketState) whose STUN phase (stunPhase) and candidate phase
// (candidatePhase) are mutually exclusive. arm() is the only transition:
// only after it may the socket send candidate packets. STUN queries
// (collectUDPProfileSample, udpMappedAddr) run only before arm, and their
// call sites keep no socket reference for later queries. Sending a
// candidate packet before arm, or arming twice, panics. This is deliberate:
// a violation surfaces immediately in development and tests instead of
// showing up as sporadic packet loss in production.
type udpSocketPhase int32

const (
	stunPhase udpSocketPhase = iota
	candidatePhase
)

// udpSocketState tracks the phase of a *net.UDPConn.
type udpSocketState struct {
	mu    sync.Mutex
	phase udpSocketPhase
}

// arm moves the socket from the STUN phase to the candidate phase. It may be
// called only once; a second call (for example after a stray STUN query in
// the candidate phase) panics, so the invariant is enforced by code rather
// than by convention.
func (s *udpSocketState) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == candidatePhase {
		panic("burrow: socket armed twice for candidate probing")
	}
	s.phase = candidatePhase
}

// assertCandidatePhase is called on the send path to check that the socket
// has left the STUN phase.
func (s *udpSocketState) assertCandidatePhase() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != candidatePhase {
		panic("burrow: candidate packet sent on a socket still in the STUN phase")
	}
}

// udpProbePayload is the fixed content of hole punching probe packets. It
// carries no application data: probes only establish the NAT mappings.
// Once the candidates are handed over, the caller sends its own protocol's
// packets over the same punched 4-tuple.
var udpProbePayload = []byte("BURROW1")

// udpPunchResult is the outcome of a successful punch.
type udpPunchResult struct {
	remote netip.AddrPort
}

// punchUDPSingle repeatedly sends probes to a single remote address until a
// packet arrives or the timeout expires. It is used by the direct strategy
// and by the predictive side that aims at the stable endpoint.
//
// state must already be armed; assertCandidatePhase checks this at runtime
// before sending.
func punchUDPSingle(ctx context.Context, state *udpSocketState, limiter *candidatePacketLimiter, conn udpPacketConn, remote netip.AddrPort, interval, timeout time.Duration) (*udpPunchResult, error) {
	state.assertCandidatePhase()
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	recvCh := make(chan netip.AddrPort, 1)
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 1500)
		for attemptCtx.Err() == nil {
			_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				continue
			}
			if n == 0 {
				continue
			}
			select {
			case recvCh <- from:
			default:
			}
			return
		}
	}()
	defer func() {
		cancel()
		<-recvDone
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if err := sendCandidatePacketConn(attemptCtx, limiter, conn, remote, udpProbePayload); err != nil && attemptCtx.Err() != nil {
		return nil, attemptCtx.Err()
	}
	for {
		select {
		case from := <-recvCh:
			return &udpPunchResult{remote: from}, nil
		case <-ticker.C:
			_ = sendCandidatePacketConn(attemptCtx, limiter, conn, remote, udpProbePayload)
		case <-attemptCtx.Done():
			select {
			case from := <-recvCh:
				return &udpPunchResult{remote: from}, nil
			default:
			}
			return nil, attemptCtx.Err()
		}
	}
}

// udpPacketConn is the subset of *net.UDPConn needed to send and receive
// candidate packets, so tests can substitute a fake connection.
type udpPacketConn interface {
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
	ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error)
	SetReadDeadline(t time.Time) error
}

func sendCandidatePacketConn(ctx context.Context, limiter *candidatePacketLimiter, conn udpPacketConn, remote netip.AddrPort, payload []byte) error {
	if limiter != nil {
		if err := limiter.wait(ctx); err != nil {
			return err
		}
	}
	_, err := conn.WriteToUDPAddrPort(payload, remote)
	return err
}

// scanUDPWindow sends probes to every address in remotes, in order, and
// repeats full rounds until the timeout. The caller decides the order (for
// predictive it is the center-out order of predictivePortSequence). It
// returns as soon as a packet arrives from any address in the window. This
// implements the stable side of the predictive strategy, which scans a
// port window centered on the peer's mapping.
//
// state must already be armed. Sending is rate limited by limiter (4096
// packets/s, burst 1024 by default), so one round over a 4096-port window
// takes a little over a second; roundInterval is the pause between rounds.
func scanUDPWindow(ctx context.Context, state *udpSocketState, limiter *candidatePacketLimiter, conn udpPacketConn, remotes []netip.AddrPort, roundInterval, timeout time.Duration) (*udpPunchResult, error) {
	state.assertCandidatePhase()
	if len(remotes) == 0 {
		return nil, fmt.Errorf("burrow: predictive scan window is empty")
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	remoteSet := make(map[netip.AddrPort]struct{}, len(remotes))
	for _, remote := range remotes {
		remoteSet[remote] = struct{}{}
	}

	recvCh := make(chan netip.AddrPort, 1)
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 1500)
		for attemptCtx.Err() == nil {
			_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				continue
			}
			if n == 0 {
				continue
			}
			if _, known := remoteSet[from]; !known {
				continue
			}
			select {
			case recvCh <- from:
			default:
			}
			return
		}
	}()
	defer func() {
		cancel()
		<-recvDone
	}()

	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		for attemptCtx.Err() == nil {
			for _, remote := range remotes {
				if err := sendCandidatePacketConn(attemptCtx, limiter, conn, remote, udpProbePayload); err != nil {
					return
				}
				if attemptCtx.Err() != nil {
					return
				}
			}
			select {
			case <-time.After(roundInterval):
			case <-attemptCtx.Done():
				return
			}
		}
	}()

	select {
	case from := <-recvCh:
		cancel()
		<-sendDone
		return &udpPunchResult{remote: from}, nil
	case <-attemptCtx.Done():
		<-sendDone
		select {
		case from := <-recvCh:
			return &udpPunchResult{remote: from}, nil
		default:
		}
		return nil, attemptCtx.Err()
	}
}
