package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startEchoServer starts a TCP echo server and returns its address.
func startEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// pipeDialer connects the client's "punch" directly to the server's accept
// through an in-memory conn pair, wiring the full path without a real
// network.
type pipeDialer struct {
	incoming chan net.Conn
}

func (d *pipeDialer) Dial(ctx context.Context) (net.Conn, error) {
	clientConn, serverConn := net.Pipe()
	select {
	case d.incoming <- serverConn:
		return clientConn, nil
	case <-ctx.Done():
		clientConn.Close()
		serverConn.Close()
		return nil, ctx.Err()
	}
}

// pipeListener hands conns from pipeDialer to the server as punched holes.
type pipeListener struct {
	incoming chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.incoming:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// setupTunnel wires client -> (in-memory hole) -> server -> echo server.
func setupTunnel(t *testing.T) (tunnelAddr string, clientTracker *statusTracker) {
	t.Helper()
	tunnelAddr, _, tracker := setupTunnelClient(t)
	return tunnelAddr, tracker
}

// setupTunnelClient is like setupTunnel but also returns the *Client, for
// tests that control or assert on Close() (for example, that it does not
// deadlock).
func setupTunnelClient(t *testing.T) (tunnelAddr string, client *Client, clientTracker *statusTracker) {
	t.Helper()
	echoAddr := startEchoServer(t)
	incoming := make(chan net.Conn)

	serverTracker := newStatusTracker("test-room", nil)
	server := &Server{
		forward:  echoAddr,
		listener: &pipeListener{incoming: incoming, closed: make(chan struct{})},
		tracker:  serverTracker,
		logger:   newLogger("error"),
		done:     make(chan struct{}),
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tracker := newStatusTracker("test-room", nil)
	logger := newLogger("error")
	client = &Client{
		listener: listener,
		tracker:  tracker,
		logger:   logger,
		sessions: newSessionManager(&pipeDialer{incoming: incoming}, tracker, logger, 0),
		done:     make(chan struct{}),
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() { client.Close() }) // Close() is idempotent; covers tests that fail early

	// The client only serves connections from pre-punched holes and closes
	// an upstream connection right away while the pool is empty (see
	// Client.handle). Start() fills the pool in the background, so a test
	// that connects immediately can race the first hole and see EOF or a
	// reset, notably under -race on a loaded machine. Wait for one hole.
	waitForIdleHole(t, client.sessions)

	return listener.Addr().String(), client, tracker
}

// waitForIdleHole waits until the background refill has put at least one
// hole into the idle pool.
func waitForIdleHole(t *testing.T, sessions *sessionManager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		sessions.mu.Lock()
		ready := len(sessions.idle) > 0
		sessions.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the idle pool did not get a pre-punched hole in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTunnelEndToEnd(t *testing.T) {
	tunnelAddr, _ := setupTunnel(t)

	conn, err := net.Dial("tcp", tunnelAddr)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	payload := []byte("raw bytes through the tunnel")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(payload, echoed) {
		t.Errorf("echo = %q, want %q", echoed, payload)
	}
}

// Each served connection gets its own hole and data never crosses between
// connections. When the pool has no hole, a connection is closed
// immediately instead of queueing for a punch, so with more concurrent
// connections than the pool holds some are rejected and the test cannot
// require every connection to be served.
//
// Rejecting instead of queueing: punching is serialized and one run can take
// hundreds of rounds (seconds to tens of seconds). Waiting would leave a
// browser spinning until it times out; closing lets the upstream reconnect
// right away while the pool refills in the background.
//
// No cross-talk is always required: a hole paired with the wrong connection
// is as bad as it gets.
func TestTunnelConcurrentConnectionsEachGetOwnHole(t *testing.T) {
	tunnelAddr, tracker := setupTunnel(t)

	const concurrency = 10
	var wg sync.WaitGroup
	var corrupted atomic.Int32
	errs := make([]error, concurrency)
	for i := range concurrency {
		wg.Go(func() {
			conn, err := net.Dial("tcp", tunnelAddr)
			if err != nil {
				errs[i] = err
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))

			payload := make([]byte, 1024)
			rand.Read(payload)
			if _, err := conn.Write(payload); err != nil {
				errs[i] = err
				return
			}
			echoed := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, echoed); err != nil {
				errs[i] = err
				return
			}
			if !bytes.Equal(payload, echoed) {
				// Cross-talk is a hard error, tracked separately so it is
				// not mistaken for a normal empty-pool rejection in errs.
				corrupted.Add(1)
			}
		})
	}
	wg.Wait()

	// Cross-talk is never acceptable.
	if got := corrupted.Load(); got != 0 {
		t.Errorf("%d connections had cross-talk: holes paired with the wrong connection", got)
	}

	// Rejections are expected (the pool is finite), but not all of them: that
	// would mean the pool does not work. Some connections must be served,
	// with intact data (checked above).
	served := 0
	for _, err := range errs {
		if err == nil {
			served++
		}
	}
	if served == 0 {
		t.Fatalf("none of %d concurrent connections were served: idle pool not working", concurrency)
	}
	// Total streams must equal the served connections: one hole each.
	if got := tracker.Snapshot().TotalStreams; got != uint64(served) {
		t.Errorf("TotalStreams = %d, want %d (served connections): one hole per connection",
			got, served)
	}
	t.Logf("%d concurrent: %d served, %d rejected on empty pool (expected)",
		concurrency, served, concurrency-served)
}

func TestTunnelLargePayload(t *testing.T) {
	tunnelAddr, _ := setupTunnel(t)

	conn, err := net.Dial("tcp", tunnelAddr)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	payload := make([]byte, 4*1024*1024) // 4 MiB
	rand.Read(payload)

	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		writeDone <- err
	}()

	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(payload, echoed) {
		t.Error("echo does not match payload for large transfer")
	}
}

// A hole is ready as soon as a connection is accepted, not deferred until the
// first write.
//
// Holes are pre-punched into a pool (startPool in session.go) when the tunnel
// starts, so an upstream connection can use one immediately with no punch
// latency. Accordingly the server only treats a candidate as dead on EOF/RST
// (server.go), so idle pooled holes are not killed.
//
// Contract: after merely opening a TCP connection, without writing a byte,
// the tunnel is already connected.
func TestTunnelPunchesOnAccept(t *testing.T) {
	tunnelAddr, tracker := setupTunnel(t)

	conn, err := net.Dial("tcp", tunnelAddr)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Write nothing; just wait for the hole.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tracker.Snapshot().State == StateConnected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := tracker.Snapshot().State; got != StateConnected {
		t.Errorf("after opening a TCP connection only, State = %q, want %q (hole should be ready on accept)",
			got, StateConnected)
	}

	// The hole works: written bytes echo back unchanged.
	conn.Write([]byte("x"))
	echoed := make([]byte, 1)
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if echoed[0] != 'x' {
		t.Errorf("echo = %q, want %q", echoed, "x")
	}
}

// Server-speaks-first upper-layer protocols must not deadlock. When the client
// connects without writing, the echo server (standing in for the protocol's
// server) sends nothing either, so the client reading nothing is expected and
// is not a deadlock.
//
// A Read timing out on the test's own SetReadDeadline proves nothing about
// relay: that deadline is on the local conn and would fire even if
// relayDeferReverse were stuck forever.
//
// Client.Close() is what exposes a deadlock: it waits in wg.Wait() for every
// handle() goroutine, so a handle() stuck in relayDeferReverse would block
// Close() forever. The test keeps a connection open without writing (server
// speaks first, client never disconnects), calls Close(), and requires it to
// return in reasonable time. This guards against relayDeferReverse hanging
// when it was never triggered and the stream side disconnected.
func TestTunnelServerSpeaksFirstDoesNotDeadlock(t *testing.T) {
	tunnelAddr, client, _ := setupTunnelClient(t)

	conn, err := net.Dial("tcp", tunnelAddr)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	// Connect, write nothing and do not close, like a client waiting for the
	// server's greeting in a server-speaks-first protocol.
	time.Sleep(100 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close() returned error: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Client.Close() deadlocked: did not return in time with a connected but silent connection; " +
			"the handle() goroutine in relayDeferReverse is blocked forever")
	}
}
