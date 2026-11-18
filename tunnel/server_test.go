package tunnel

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fanoutListener simulates birthday fanout: Accept returns several candidate
// connections in turn. Only some are live (the peer actually sends bytes);
// the rest are dead (the peer connects but never sends a byte or
// disconnects).
type fanoutListener struct {
	mu       sync.Mutex
	conns    []net.Conn
	closed   chan struct{}
	closeVal sync.Once
}

func newFanoutListener(conns []net.Conn) *fanoutListener {
	return &fanoutListener{conns: conns, closed: make(chan struct{})}
}

func (l *fanoutListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.conns) == 0 {
		l.mu.Unlock()
		<-l.closed
		return nil, net.ErrClosed
	}
	conn := l.conns[0]
	l.conns = l.conns[1:]
	l.mu.Unlock()
	return conn, nil
}

func (l *fanoutListener) Close() error {
	l.closeVal.Do(func() { close(l.closed) })
	return nil
}

// newCandidatePair returns an in-memory conn pair simulating one accepted
// punch candidate. serverSide goes to Server.acceptLoop; the test decides
// whether to write to peerSide (writing makes it live, silence makes it a
// zombie).
func newCandidatePair() (serverSide, peerSide net.Conn) {
	return net.Pipe()
}

// closeTrackingConn wraps a net.Conn and records whether Close() was called.
//
// Recording Close() on the server-side conn is the most direct evidence that
// the server reaped a zombie candidate, independent of what the peer can
// observe.
type closeTrackingConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// Of several candidates only one sends bytes (the single surviving fanout
// candidate). The others must be closed after the zombie timeout, leaking no
// goroutine or socket, while the live candidate keeps forwarding and is the
// only one counted.
//
// A dead candidate is one that connects but never sends a byte (see
// zombieCandidateTimeout in server.go).
func TestServerReapsZombieCandidates(t *testing.T) {
	echoAddr := startEchoServer(t)

	const deadCandidates = 3
	var serverConns []net.Conn
	var trackedDead []*closeTrackingConn
	var peerConns []net.Conn
	for range deadCandidates {
		s, p := newCandidatePair()
		tracked := &closeTrackingConn{Conn: s}
		serverConns = append(serverConns, tracked)
		trackedDead = append(trackedDead, tracked)
		peerConns = append(peerConns, p)
	}
	liveServerConn, livePeerConn := newCandidatePair()
	serverConns = append(serverConns, liveServerConn)
	peerConns = append(peerConns, livePeerConn)

	tracker := newStatusTracker("test-room", nil)
	server := &Server{
		forward:       echoAddr,
		listener:      newFanoutListener(serverConns),
		tracker:       tracker,
		logger:        newLogger("error"),
		done:          make(chan struct{}),
		zombieTimeout: 150 * time.Millisecond, // short test timeout; production default is zombieCandidateTimeout
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	// Dead candidates: the peer connects and never sends or disconnects. No
	// protocol is needed on the peer; holes carry raw bytes.
	defer func() {
		for i := range deadCandidates {
			peerConns[i].Close()
		}
	}()

	// Live candidate: write raw bytes into the hole and verify the echo.
	stream := livePeerConn
	defer stream.Close()

	stream.SetDeadline(time.Now().Add(5 * time.Second))
	payload := []byte("live candidate payload")
	if _, err := stream.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echoed := make([]byte, len(payload))
	if _, err := readFull(stream, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echoed) != string(payload) {
		t.Errorf("echo = %q, want %q", echoed, payload)
	}

	// The live candidate shows up immediately (SessionCount = 1, live only).
	if got := tracker.Snapshot().SessionCount; got != 1 {
		t.Errorf("after live candidate SessionCount = %d, want 1", got)
	}

	// After the zombie timeout every dead candidate's conn must have been
	// closed by the server, as recorded by closeTrackingConn.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allClosed := true
		for _, c := range trackedDead {
			if !c.closed.Load() {
				allClosed = false
				break
			}
		}
		if allClosed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i, c := range trackedDead {
		if !c.closed.Load() {
			t.Errorf("dead candidate %d not closed by server after zombie timeout: goroutine/socket leak", i)
		}
	}

	// Reaping dead candidates must not affect the live one: count stays 1 and
	// the hole still works.
	if got := tracker.Snapshot().SessionCount; got != 1 {
		t.Errorf("after reaping SessionCount = %d, want 1 (live candidate unaffected)", got)
	}
	if _, err := stream.Write([]byte("y")); err != nil {
		t.Errorf("live hole should still be writable after reaping: %v", err)
	}
}

func readFull(r net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// A sole candidate that has sent bytes must not be reaped as a zombie.
func TestServerDoesNotReapLiveSoleCandidate(t *testing.T) {
	echoAddr := startEchoServer(t)
	serverConn, peerConn := newCandidatePair()

	tracker := newStatusTracker("test-room", nil)
	server := &Server{
		forward:       echoAddr,
		listener:      newFanoutListener([]net.Conn{serverConn}),
		tracker:       tracker,
		logger:        newLogger("error"),
		done:          make(chan struct{}),
		zombieTimeout: 150 * time.Millisecond,
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	defer peerConn.Close()

	// Send bytes so the server considers the candidate live; this must happen
	// within the zombie timeout.
	peerConn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := peerConn.Write([]byte("hello")); err != nil {
		t.Fatalf("write first byte: %v", err)
	}
	// Read the echo to confirm the server connected the candidate to the
	// forward target (i.e. considers it live).
	echoed := make([]byte, 5)
	if _, err := readFull(peerConn, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	// Wait past the zombie timeout and confirm the live candidate survives.
	time.Sleep(300 * time.Millisecond)

	peerConn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := peerConn.Write([]byte("still alive")); err != nil {
		t.Fatalf("hole should still be writable after timeout window, got: %v", err)
	}
	// Read the echo again: if the hole was wrongly reaped this fails or times out.
	echoed2 := make([]byte, len("still alive"))
	if _, err := readFull(peerConn, echoed2); err != nil {
		t.Errorf("sole live candidate was wrongly reaped as a zombie: %v", err)
	}
}
