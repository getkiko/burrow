package burrow

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// newTestPuncher builds a puncher that only uses the passive direction: it
// listens on loopback and dials a black-hole address, so dialing never
// produces a candidate and accCh behavior can be tested in isolation.
func newTestPuncher(t *testing.T, opt punchOptions) *puncher {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &puncher{
		localAddr: ln.Addr().String(),
		publicTCP: "203.0.113.1:1111",
		self:      roleListener,
		session:   "testsess",
		opt:       opt,
		ln:        ln,
		accCh:     make(chan net.Conn, accChCapacity),
		cancel:    cancel,
	}
	p.wg.Go(func() {
		<-ctx.Done()
		_ = p.ln.Close()
	})
	p.wg.Go(func() { p.acceptLoop(ctx) })
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func testOpt() punchOptions {
	return punchOptions{
		punchTimeout: 2 * time.Second,
		dialTimeout:  100 * time.Millisecond,
		dialInterval: 50 * time.Millisecond,
	}
}

// blackhole is an unroutable address (TEST-NET-3); dialing it always fails.
const blackhole = "203.0.113.9:9"

// punch delivers a passive connection from the expected source.
func TestPunchDeliversPassiveConn(t *testing.T) {
	p := newTestPuncher(t, testOpt())

	// Dial the puncher's listening port from loopback; the source IP is 127.0.0.1.
	go func() {
		time.Sleep(50 * time.Millisecond)
		c, err := net.Dial("tcp4", p.localAddr)
		if err == nil {
			time.Sleep(time.Second)
			_ = c.Close()
		}
	}()

	// The connection is claimed only if peer's IP matches the source.
	res, err := p.punch(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatalf("want passive connection claimed, got %v", err)
	}
	defer res.conn.Close()
	if !res.passive {
		t.Error("result should be marked passive")
	}
}

// punch drops connections from a mismatched source IP and eventually times out.
func TestPunchRejectsWrongSource(t *testing.T) {
	p := newTestPuncher(t, testOpt())

	go func() {
		time.Sleep(50 * time.Millisecond)
		c, err := net.Dial("tcp4", p.localAddr)
		if err == nil {
			time.Sleep(time.Second)
			_ = c.Close()
		}
	}()

	// The expected peer is 203.0.113.7 but the source is 127.0.0.1, so it must be rejected.
	_, err := p.punch(context.Background(), blackhole)
	if !errors.Is(err, ErrPunchFailed) {
		t.Fatalf("punch should fail for a mismatched source, got %v", err)
	}
}

// Without any candidate, punch returns ErrPunchFailed after punchTimeout.
func TestPunchTimesOut(t *testing.T) {
	p := newTestPuncher(t, testOpt())
	start := time.Now()
	_, err := p.punch(context.Background(), blackhole)
	if !errors.Is(err, ErrPunchFailed) {
		t.Fatalf("want ErrPunchFailed, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

// punch returns promptly when the outer ctx is canceled, without waiting for punchTimeout.
func TestPunchHonorsContextCancel(t *testing.T) {
	opt := testOpt()
	opt.punchTimeout = 30 * time.Second
	p := newTestPuncher(t, opt)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := p.punch(ctx, blackhole)
	if err == nil {
		t.Fatal("punch should fail after ctx is canceled")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("punch did not return promptly after cancel: %v", elapsed)
	}
}

// punchCandidates delivers a passive candidate and stops when yield returns an error.
func TestPunchCandidatesYieldError(t *testing.T) {
	p := newTestPuncher(t, testOpt())

	go func() {
		time.Sleep(50 * time.Millisecond)
		c, err := net.Dial("tcp4", p.localAddr)
		if err == nil {
			time.Sleep(time.Second)
			_ = c.Close()
		}
	}()

	sentinel := errors.New("stop")
	err := p.punchCandidates(context.Background(), "127.0.0.1:1", 2, 0, func(r *punchResult) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("yield error should propagate, got %v", err)
	}
}

// punchCandidates returns ErrPunchFailed without candidates.
func TestPunchCandidatesTimeout(t *testing.T) {
	p := newTestPuncher(t, testOpt())
	err := p.punchCandidates(context.Background(), blackhole, 2, 0, func(r *punchResult) error {
		_ = r.conn.Close()
		return nil
	})
	if !errors.Is(err, ErrPunchFailed) {
		t.Fatalf("want ErrPunchFailed, got %v", err)
	}
}

// acceptLoop drops extra connections when accCh is full and never blocks.
func TestAcceptLoopDoesNotBlockWhenFull(t *testing.T) {
	p := newTestPuncher(t, testOpt())

	var wg sync.WaitGroup
	for range accChCapacity + 4 {
		wg.Go(func() {
			c, err := net.Dial("tcp4", p.localAddr)
			if err == nil {
				time.Sleep(300 * time.Millisecond)
				_ = c.Close()
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("acceptLoop blocked with a full queue")
	}
}

// Close is idempotent and does not leak.
func TestPuncherCloseIdempotent(t *testing.T) {
	p := newTestPuncher(t, testOpt())
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("repeated Close should be safe, got %v", err)
	}
}

// Edge cases of splitHostPortInt.
func TestSplitHostPortInt(t *testing.T) {
	h, port, err := splitHostPortInt("1.2.3.4:9960")
	if err != nil || h != "1.2.3.4" || port != 9960 {
		t.Fatalf("parse failed: %q %d %v", h, port, err)
	}
	for _, bad := range []string{"1.2.3.4", "1.2.3.4:0", "1.2.3.4:70000", "1.2.3.4:x"} {
		if _, _, err := splitHostPortInt(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestSameHost(t *testing.T) {
	if !sameHost("1.2.3.4:1", "1.2.3.4:2") {
		t.Error("same IP with different ports should match")
	}
	if sameHost("1.2.3.4:1", "5.6.7.8:1") {
		t.Error("different IPs should not match")
	}
	if sameHost("bad", "1.2.3.4:1") {
		t.Error("invalid address should not match")
	}
	if !sameHost("[::1]:1", "[0:0:0:0:0:0:0:1]:2") {
		t.Error("equivalent IPv6 addresses should match")
	}
}

func TestAllocLocalPortIPv6(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	_ = probe.Close()

	localAddr, err := allocLocalPort("::1", "tcp6", nil)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(localAddr)
	if err != nil {
		t.Fatalf("malformed IPv6 local address %q: %v", localAddr, err)
	}
	if host != "::1" || port == "0" {
		t.Fatalf("IPv6 local address = %q", localAddr)
	}
}

func TestNewPuncherIPv6Listener(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	_ = probe.Close()

	p, err := newPuncher(context.Background(), "[::1]:0", "[2001:db8::1]:1234", roleDialer, "ipv6-test", testOpt())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.network != "tcp6" {
		t.Fatalf("listen network = %q", p.network)
	}
	addr := p.ln.Addr().(*net.TCPAddr)
	if addr.IP.String() != "::1" || addr.Port == 0 {
		t.Fatalf("listen address = %s", addr)
	}
}
