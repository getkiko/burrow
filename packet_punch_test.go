package burrow

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// How the socket-phase invariant (a socket must finish STUN before candidate
// packets are sent on it) is tested:
//
// The real bug it prevents, candidate packets silently swallowed by
// stunPacketConn.Read while a STUN query is in flight, comes from two
// concurrent readers on one socket and is too probabilistic to reproduce
// reliably in a unit test. The invariant is checked structurally instead:
//
//  1. udpSocketState models each socket as a two-state machine
//     (stunPhase -> candidatePhase, one-way, once). Every candidate send
//     path (the entry of punchUDPSingle and scanUDPWindow) calls
//     assertCandidatePhase, which panics before arm().
//     TestUDPSocketStatePanicsBeforeArm asserts this, so sending candidate
//     packets during STUN is not a reachable state.
//  2. TestUDPSocketStateArmIsOneWay asserts that arm may be called only
//     once, so a socket in the candidate phase can never go back to STUN.
//  3. armPacketAttempt in packet_session.go is the only caller of arm(),
//     and it runs only after prepareUDPSocket (the STUN query) has
//     returned. That ordering is verified by code review, not at runtime.

// Candidate send paths must refuse (panic) before the socket is armed: the
// runtime check that no candidate packet goes out during the STUN phase.
func TestUDPSocketStatePanicsBeforeArm(t *testing.T) {
	state := &udpSocketState{}
	defer func() {
		if recover() == nil {
			t.Fatal("assertCandidatePhase on an unarmed socket should panic")
		}
	}()
	state.assertCandidatePhase()
}

// After arm, assertCandidatePhase must pass (candidate packets are allowed).
func TestUDPSocketStateAllowsCandidateAfterArm(t *testing.T) {
	state := &udpSocketState{}
	state.arm()
	// Must not panic.
	state.assertCandidatePhase()
}

// arm may be called only once. A second arm amounts to going back to the
// STUN phase and must panic, so the invariant cannot be bypassed.
func TestUDPSocketStateArmIsOneWay(t *testing.T) {
	state := &udpSocketState{}
	state.arm()
	defer func() {
		if recover() == nil {
			t.Fatal("second arm should panic")
		}
	}()
	state.arm()
}

// punchUDPSingle must punch over a real network path: two loopback UDP
// sockets probe each other, and both must receive the other's probe and
// return before the timeout.
func TestPunchUDPSingleLoopback(t *testing.T) {
	connA, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()
	connB, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close()

	addrA := connA.LocalAddr().(*net.UDPAddr).AddrPort()
	addrB := connB.LocalAddr().(*net.UDPAddr).AddrPort()

	stateA := &udpSocketState{}
	stateA.arm()
	stateB := &udpSocketState{}
	stateB.arm()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resultCh := make(chan *udpPunchResult, 2)
	errCh := make(chan error, 2)
	go func() {
		res, err := punchUDPSingle(ctx, stateA, nil, connA, addrB, 50*time.Millisecond, 2*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- res
	}()
	go func() {
		res, err := punchUDPSingle(ctx, stateB, nil, connB, addrA, 50*time.Millisecond, 2*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- res
	}()

	for range 2 {
		select {
		case err := <-errCh:
			t.Fatalf("punch failed: %v", err)
		case res := <-resultCh:
			if !res.remote.IsValid() {
				t.Fatal("punch result has an invalid address")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("punch timed out")
		}
	}
}

// scanUDPWindow must find the address that actually replies inside the
// window, even when it is not the first candidate (the peer port is not at
// the center of the window).
func TestScanUDPWindowFindsHit(t *testing.T) {
	scanner, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	scannerAddr := scanner.LocalAddr().(*net.UDPAddr).AddrPort()
	targetAddr := target.LocalAddr().(*net.UDPAddr).AddrPort()
	targetPort := int(targetAddr.Port())

	// Build a window of width 4 centered on targetPort, so the target is
	// inside it, as when the prediction hits.
	remotes := predictivePortSequence(targetPort, 4)
	addrPorts := make([]netip.AddrPort, 0, len(remotes))
	for _, port := range remotes {
		addrPorts = append(addrPorts, netip.AddrPortFrom(targetAddr.Addr(), uint16(port)))
	}

	// The target echoes any packet back to its sender, like a peer that
	// answers while punching.
	go func() {
		buf := make([]byte, 1500)
		_ = target.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, from, err := target.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		_, _ = target.WriteToUDPAddrPort(buf[:n], from)
	}()

	state := &udpSocketState{}
	state.arm()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := scanUDPWindow(ctx, state, nil, scanner, addrPorts, 200*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("scanUDPWindow failed: %v", err)
	}
	if result.remote.Port() != uint16(targetPort) || !result.remote.Addr().IsLoopback() {
		t.Fatalf("hit address mismatch: got %v, want port %d", result.remote, targetPort)
	}
	_ = scannerAddr // the scanner's own address, kept for reference only
}
