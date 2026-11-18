package burrow

import (
	"context"
	"testing"
	"time"
)

func testPacketPairConfig(t *testing.T, signalAddr, stunAddr string) PacketConfig {
	t.Helper()
	return PacketConfig{
		SignalAddr: signalAddr,
		Room:       "e2e-room",
		STUN:       STUNOptions{Servers: []string{stunAddr}, Timeout: time.Second},
		Strategy:   StrategyDirect,
		Control:    nil,
		DebugLog:   func(format string, args ...any) { t.Logf(format, args...) },
	}
}

// End to end: with the direct strategy, PacketListener.AcceptPacket and
// PacketDialer.DialPacket must complete a full punching round with each
// other and each get a PacketAttempt.
//
// Both sides bind 127.0.0.1 and the mock STUN server echoes the caller's
// real source address (what a loopback interface without NAT would
// report), so candidate probes are plain loopback UDP and the punch
// succeeds reliably without public STUN or NAT.
func TestPacketDialerListenerRoundTrip(t *testing.T) {
	signalServer := startMockUDPSignalServer(t)
	echoSTUN := startEchoSTUNServer(t)

	listenerCfg := testPacketPairConfig(t, signalServer.addr(), echoSTUN.addr)
	dialerCfg := testPacketPairConfig(t, signalServer.addr(), echoSTUN.addr)

	listener, err := ListenPacket(listenerCfg)
	if err != nil {
		t.Fatalf("ListenPacket failed: %v", err)
	}
	defer listener.Close()

	// Give the listener time to register with LISTEN.
	time.Sleep(150 * time.Millisecond)

	dialer, err := NewPacketDialer(dialerCfg)
	if err != nil {
		t.Fatalf("NewPacketDialer failed: %v", err)
	}
	defer dialer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	acceptCh := make(chan *PacketAttempt, 1)
	acceptErrCh := make(chan error, 1)
	go func() {
		attempt, err := listener.AcceptPacket(ctx)
		if err != nil {
			acceptErrCh <- err
			return
		}
		acceptCh <- attempt
	}()

	dialAttempt, err := dialer.DialPacket(ctx)
	if err != nil {
		t.Fatalf("DialPacket failed: %v", err)
	}
	defer dialAttempt.Close()

	if dialAttempt.Strategy != StrategyDirect {
		t.Errorf("Strategy = %v, want direct", dialAttempt.Strategy)
	}
	if len(dialAttempt.Paths) != 1 {
		t.Fatalf("direct strategy should have exactly one path, got %d", len(dialAttempt.Paths))
	}

	select {
	case err := <-acceptErrCh:
		t.Fatalf("AcceptPacket failed: %v", err)
	case listenAttempt := <-acceptCh:
		defer listenAttempt.Close()
		if listenAttempt.Strategy != StrategyDirect {
			t.Errorf("listener Strategy = %v, want direct", listenAttempt.Strategy)
		}
		if len(listenAttempt.Paths) != 1 {
			t.Fatalf("listener direct strategy should have exactly one path, got %d", len(listenAttempt.Paths))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AcceptPacket timed out")
	}
}

// End to end with the predictive strategy: the listener (stable side) scans
// a port window centered on the dialer's mapping, and the dialer (changing
// side) aims at the listener's stable endpoint. The window is small (8) to
// keep the test fast and deterministic; the hit is the window center (the
// peer's real port). This checks that the role split and the candidate
// send path actually get through on a real network.
func TestPacketDialerListenerPredictiveRoundTrip(t *testing.T) {
	signalServer := startMockUDPSignalServer(t)
	echoSTUN := startEchoSTUNServer(t)

	predictive := PredictiveOptions{PortScanRange: 8, Concurrency: 4, AttemptTimeout: 3 * time.Second}
	listenerCfg := testPacketPairConfig(t, signalServer.addr(), echoSTUN.addr)
	listenerCfg.Strategy = StrategyPredictive
	listenerCfg.StableSide = true
	listenerCfg.Predictive = predictive
	dialerCfg := testPacketPairConfig(t, signalServer.addr(), echoSTUN.addr)
	dialerCfg.Strategy = StrategyPredictive
	dialerCfg.Predictive = predictive

	listener, err := ListenPacket(listenerCfg)
	if err != nil {
		t.Fatalf("ListenPacket failed: %v", err)
	}
	defer listener.Close()
	time.Sleep(150 * time.Millisecond)

	dialer, err := NewPacketDialer(dialerCfg)
	if err != nil {
		t.Fatalf("NewPacketDialer failed: %v", err)
	}
	defer dialer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	acceptCh := make(chan *PacketAttempt, 1)
	acceptErrCh := make(chan error, 1)
	go func() {
		attempt, err := listener.AcceptPacket(ctx)
		if err != nil {
			acceptErrCh <- err
			return
		}
		acceptCh <- attempt
	}()

	dialAttempt, err := dialer.DialPacket(ctx)
	if err != nil {
		t.Fatalf("DialPacket failed: %v", err)
	}
	defer dialAttempt.Close()
	if dialAttempt.Strategy != StrategyPredictive {
		t.Errorf("Strategy = %v, want predictive", dialAttempt.Strategy)
	}
	// The dialer (changing side) aims at one address: one Remote.
	if len(dialAttempt.Paths) != 1 || len(dialAttempt.Paths[0].Remotes) != 1 {
		t.Fatalf("dialer predictive should have a single exact address, got %+v", dialAttempt.Paths)
	}

	select {
	case err := <-acceptErrCh:
		t.Fatalf("AcceptPacket failed: %v", err)
	case listenAttempt := <-acceptCh:
		defer listenAttempt.Close()
		if listenAttempt.Strategy != StrategyPredictive {
			t.Errorf("listener Strategy = %v, want predictive", listenAttempt.Strategy)
		}
		// The listener (stable side) scans a window: scanRange+1 Remotes.
		if len(listenAttempt.Paths) != 1 || len(listenAttempt.Paths[0].Remotes) != predictive.PortScanRange+1 {
			t.Fatalf("listener predictive scan window size mismatch, got %+v", listenAttempt.Paths)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("AcceptPacket timed out")
	}
}
