package burrow

import (
	"context"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// After a hit, scanDial must cancel the rest of the round at once instead of
// letting each dial run until scanDialTimeout. Otherwise an early hit in a
// 512-port round would still return late and pile up useless SYN_SENT
// sockets in the kernel.
//
// The test listens on a real loopback port as the hit target and uses a wide
// window in which almost every other port has no listener, then checks that
// scanDial returns quickly after the hit.
func TestScanDialStopsAfterHit(t *testing.T) {
	// Hit target: a loopback port that accepts connections.
	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	hitPort := target.Addr().(*net.TCPAddr).Port

	// Local socket address.
	local, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	localAddr := local.Addr().String()
	_ = local.Close()

	p := &puncher{
		localAddr: localAddr,
		self:      roleDialer,
		session:   "scantest",
		opt: punchOptions{
			punchTimeout:  10 * time.Second,
			portScanRange: 400,
		},
		accCh: make(chan net.Conn, accChCapacity),
	}

	// Use the hit port as basePort; the window is centered on it, so the hit
	// comes early.
	peer := net.JoinHostPort("127.0.0.1", strconv.Itoa(hitPort))

	dialCh := make(chan net.Conn, 1)
	var dialErr atomic.Pointer[error]

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		p.scanDial(ctx, peer, dialCh, &dialErr)
		close(done)
	}()

	select {
	case c := <-dialCh:
		defer c.Close()
	case <-time.After(15 * time.Second):
		t.Fatal("scan found no target")
	}

	// After the hit scanDial should exit quickly.
	select {
	case <-done:
		elapsed := time.Since(start)
		t.Logf("hit and exit took %v", elapsed)
		// Without early cancellation, 400 ports at up to scanDialTimeout
		// (300ms) each with concurrency 64 take at least (400/64)*300ms,
		// about 1.9s. Early cancellation should be much faster.
		if elapsed > 5*time.Second {
			t.Errorf("scanDial did not exit promptly after the hit, took %v", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("scanDial did not exit after the hit; the rest of the round was not canceled")
	}
}
