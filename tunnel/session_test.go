package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkiko/burrow"
)

// fakeDialer simulates punching with an in-memory conn pair and echoes data
// back on the far end.
//
// Without smux the peer needs no protocol layer: a hole carries raw bytes, so
// echoing is just io.Copy(conn, conn).
type fakeDialer struct {
	mu        sync.Mutex
	dialCount atomic.Int32
	failWith  error
	failUntil int32 // if >0, the first failUntil Dials fail and later ones succeed
	delay     time.Duration
	conns     []net.Conn
}

func (d *fakeDialer) Dial(ctx context.Context) (net.Conn, error) {
	trial := d.dialCount.Add(1)
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	failure := d.failWith
	failUntil := d.failUntil
	d.mu.Unlock()
	// When failUntil is set, the trial number decides the outcome,
	// overriding the fixed failWith behavior.
	if failUntil > 0 {
		if trial <= failUntil {
			return nil, failure
		}
	} else if failure != nil {
		return nil, failure
	}

	clientConn, serverConn := net.Pipe()
	d.mu.Lock()
	d.conns = append(d.conns, clientConn, serverConn)
	d.mu.Unlock()

	// Echo: bytes in the hole are written back unchanged, no protocol layer.
	go func() {
		defer serverConn.Close()
		io.Copy(serverConn, serverConn)
	}()
	return clientConn, nil
}

func (d *fakeDialer) setFailure(err error) {
	d.mu.Lock()
	d.failWith = err
	d.mu.Unlock()
}

// failUntilTrial makes the first n Dials fail and Dial n+1 onward succeed.
//
// It switches on the call count rather than time: failed punches are retried
// immediately (microseconds apart), so a time.Sleep-based switch would lose
// the race against the whole retry run.
func (d *fakeDialer) failUntilTrial(n int32) {
	d.mu.Lock()
	d.failUntil = n
	d.mu.Unlock()
}

// closeAll closes every fake conn, simulating holes dropping.
func (d *fakeDialer) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		c.Close()
	}
	d.conns = nil
}

func (d *fakeDialer) cleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(d.closeAll)
}

func newTestManager(t *testing.T, dialer punchDialer) (*sessionManager, *statusTracker) {
	t.Helper()
	tracker := newStatusTracker("test-room", nil)
	mgr := newSessionManager(dialer, tracker, newLogger("error"), 0)
	t.Cleanup(func() { mgr.Close() })
	return mgr, tracker
}

func TestSessionManagerOpenStream(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	mgr, tracker := newTestManager(t, dialer)

	stream, err := mgr.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	defer stream.Close()

	if got := dialer.dialCount.Load(); got != 1 {
		t.Errorf("punched %d times, want 1", got)
	}
	if got := tracker.Snapshot().State; got != StateConnected {
		t.Errorf("State = %q, want %q", got, StateConnected)
	}

	// Check that the stream works (echo).
	stream.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q", buf)
	}
}

// Each upstream connection gets its own hole: one-to-one, no sharing.
//
// This test used to assert that concurrent first connections collapse into a
// single punch, a requirement from the shared smux session era. Without
// multiplexing, N concurrent connections must punch N holes.
func TestSessionManagerConcurrentFirstConnect(t *testing.T) {
	dialer := &fakeDialer{delay: 50 * time.Millisecond}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	const concurrency = 20
	var wg sync.WaitGroup
	streams := make([]net.Conn, concurrency)
	errs := make([]error, concurrency)
	for i := range concurrency {
		wg.Go(func() {
			streams[i], errs[i] = mgr.OpenStream(t.Context())
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("OpenStream #%d failed: %v", i, err)
		}
		defer streams[i].Close()
	}
	if got := dialer.dialCount.Load(); int(got) != concurrency {
		t.Errorf("punched %d times, want %d: each connection must punch its own hole", got, concurrency)
	}
}

// Every OpenStream punches a new hole instead of reusing an existing one.
//
// This used to be TestSessionManagerReusesSession, asserting that 5
// OpenStream calls punched once (shared session). The requirement is now
// reversed: one hole per connection means 5 calls punch 5 holes.
func TestSessionManagerPunchesPerConn(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	const rounds = 5
	for range rounds {
		stream, err := mgr.OpenStream(t.Context())
		if err != nil {
			t.Fatalf("OpenStream failed: %v", err)
		}
		stream.Close()
	}
	if got := dialer.dialCount.Load(); int(got) != rounds {
		t.Errorf("punched %d times, want %d: one hole per connection, no reuse", got, rounds)
	}
}

// After a hole drops, the next OpenStream punches again, unaffected by the
// dead hole.
func TestSessionManagerRebuildsAfterClose(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	stream, err := mgr.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("first OpenStream failed: %v", err)
	}
	stream.Close()

	dialer.closeAll() // simulate the hole dropping
	time.Sleep(50 * time.Millisecond)

	stream2, err := mgr.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream after rebuild failed: %v", err)
	}
	defer stream2.Close()

	if got := dialer.dialCount.Load(); got != 2 {
		t.Errorf("punched %d times, want 2 (first + rebuild)", got)
	}
}

// A failed punch must be retried immediately, not backed off: birthday relies
// on many collision rounds, and backoff defeats that (cmd/burrow's birthday
// experiment runs 100 trials by default).
func TestSessionManagerRetriesImmediatelyOnFailure(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.setFailure(errors.New("punch timed out"))
	mgr, tracker := newTestManager(t, dialer)

	startedAt := time.Now()
	if _, err := mgr.OpenStream(t.Context()); err == nil {
		t.Fatal("OpenStream should return an error when every round fails")
	}
	elapsed := time.Since(startedAt)

	// It should have tried defaultTrials rounds back to back, not backed off
	// after one failure.
	if got := dialer.dialCount.Load(); int(got) != defaultTrials {
		t.Errorf("punched %d times, want %d (back-to-back retries)", got, defaultTrials)
	}
	// Plain failures do not wait, so 100 rounds finish quickly; any
	// second-scale backoff would blow this limit.
	if elapsed > 5*time.Second {
		t.Errorf("%d rounds took %v, too slow: backoff suspected", defaultTrials, elapsed)
	}
	if got := tracker.Snapshot().State; got != StatePunchFailed {
		t.Errorf("State = %q, want %q", got, StatePunchFailed)
	}
}

// Calling OpenStream again after exhausting all rounds starts a fresh run;
// there is no refusal window.
func TestSessionManagerRetriesAgainAfterExhaustion(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.setFailure(errors.New("punch timed out"))
	mgr, _ := newTestManager(t, dialer)

	mgr.OpenStream(t.Context())
	first := dialer.dialCount.Load()

	mgr.OpenStream(t.Context())
	second := dialer.dialCount.Load()

	if second-first != int32(defaultTrials) {
		t.Errorf("second OpenStream punched only %d times, want %d: there must be no backoff window",
			second-first, defaultTrials)
	}
}

// With ErrNoListener (peer offline) an immediate retry is pointless, so it
// waits a fixed interval that does not grow.
func TestSessionManagerWaitsOnNoListener(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.setFailure(burrow.ErrNoListener)
	tracker := newStatusTracker("r", nil)
	mgr := newSessionManager(dialer, tracker, newLogger("error"), 0)
	t.Cleanup(func() { mgr.Close() })

	// Cut off with a ctx timeout; otherwise this would take
	// defaultTrials * idleRetryDelay.
	ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
	defer cancel()

	startedAt := time.Now()
	mgr.OpenStream(ctx)
	elapsed := time.Since(startedAt)

	// Waiting 1s per round, only about 2-3 rounds fit in 2.5s, proving that
	// it waits.
	got := dialer.dialCount.Load()
	if got > 4 {
		t.Errorf("with ErrNoListener, %v elapsed and %d punches, want about 2-3 (waiting %v per round)",
			elapsed, got, idleRetryDelay)
	}
	if got == 0 {
		t.Error("should try at least once")
	}
}

// Failures then success: a success mid-run returns immediately, regardless of
// earlier failures.
func TestSessionManagerSucceedsAfterFailures(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	dialer.setFailure(errors.New("temporary failure"))
	mgr, tracker := newTestManager(t, dialer)

	// Fail rounds 1-3 and let round 4 through. Switch on count, not time:
	// failed retries are microseconds apart and a time-based switch would
	// lose the race.
	dialer.failUntilTrial(3)

	stream, err := mgr.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream should succeed once dials start succeeding: %v", err)
	}
	defer stream.Close()

	if got := tracker.Snapshot().State; got != StateConnected {
		t.Errorf("State = %q, want %q", got, StateConnected)
	}
}

func TestSessionManagerCloseStopsFurtherDials(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	tracker := newStatusTracker("r", nil)
	mgr := newSessionManager(dialer, tracker, newLogger("error"), 0)

	stream, err := mgr.OpenStream(t.Context())
	if err != nil {
		t.Fatalf("OpenStream failed: %v", err)
	}
	stream.Close()
	mgr.Close()

	if _, err := mgr.OpenStream(t.Context()); err == nil {
		t.Error("OpenStream should fail after Close")
	}
}

func TestLazyConnDefersUntilWrite(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	conn := newLazyConn(mgr)
	defer conn.Close()

	if got := dialer.dialCount.Load(); got != 0 {
		t.Fatalf("creating a lazyConn punched %d times, want 0", got)
	}

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if got := dialer.dialCount.Load(); got != 1 {
		t.Errorf("punched %d times after Write, want 1", got)
	}
}

// Read must also trigger the punch: with a server-speaks-first protocol,
// hooking only Write would deadlock.
func TestLazyConnTriggersOnRead(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	conn := newLazyConn(mgr)
	defer conn.Close()

	go func() {
		time.Sleep(100 * time.Millisecond)
		conn.Write([]byte("x")) // give the echo something to send back
	}()

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if got := dialer.dialCount.Load(); got != 1 {
		t.Errorf("punched %d times, want 1", got)
	}
}

// The punch is triggered only once, however many times Read/Write are called.
//
// The echo must be read concurrently with writing: the fakeDialer peer is
// net.Pipe plus an io.Copy echo, and net.Pipe is unbuffered. If nobody reads
// the echoed bytes, the echo side's Write blocks, stalling its read loop, and
// the third Write times out. That is a property of the in-memory pipe, not of
// the punch logic under test (smux's internal buffering used to hide it).
func TestLazyConnTriggersOnce(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	conn := newLazyConn(mgr)
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Drain the echo while writing so the unbuffered pipe does not deadlock.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		io.ReadFull(conn, make([]byte, 3))
	}()

	for range 3 {
		if _, err := conn.Write([]byte("x")); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}
	<-drained

	if got := dialer.dialCount.Load(); got != 1 {
		t.Errorf("punched %d times, want 1", got)
	}
}

func TestLazyConnCloseBeforeUse(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	conn := newLazyConn(mgr)
	if err := conn.Close(); err != nil {
		t.Errorf("Close on an unused conn should succeed, got %v", err)
	}
	if got := dialer.dialCount.Load(); got != 0 {
		t.Errorf("Close triggered %d punches, want 0", got)
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Error("Write should fail after Close")
	}
}

// Concurrent Close() racing with Write()/ensure(): reproduces the hole leak.
//
// The bug: ensure() assigned the hole from mgr.OpenStream to c.stream inside
// once.Do but outside c.mu, while Close() read c.stream under c.mu. If Close()
// ran in the window after OpenStream returned but before the assignment, it
// saw a nil c.stream and closed nothing; ensure() then attached the real hole
// to c.stream, and nobody ever closed it, leaking a punched TCP connection
// and the goroutine blocked on it.
//
// Leak detection changed with the architecture: it used to rely on the
// shared session's NumStreams() never decreasing. Without smux there is no
// session or stream count, so the test checks the in-use holes the
// sessionManager registers (mgr.conns). By the end of each round the lazyConn
// is closed, so its hole must be unregistered; anything left is a leak. This
// is more direct than NumStreams(): it counts exactly the holes nobody closed.
//
// dialer.delay widens the first-punch window so Close() can run while
// OpenStream is in progress, repeated over many rounds to hit the race; -race
// catches the data race itself.
func TestLazyConnConcurrentCloseAndWrite(t *testing.T) {
	dialer := &fakeDialer{delay: 20 * time.Millisecond}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	const rounds = 50
	for i := range rounds {
		conn := newLazyConn(mgr)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			conn.Write([]byte("x"))
		}()
		go func() {
			defer wg.Done()
			// Deliberately no sleep: aim Close at the window inside ensure()
			// after connecting but before assigning c.stream.
			conn.Close()
		}()
		wg.Wait()

		// Whoever wins, the hole obtained by ensure() must end up closed and
		// unregistered: either Close() closes it directly, or Close() runs
		// first and ensure() rechecks closed under the lock and closes the
		// fresh hole itself.
		//
		// Give the close a moment to take effect (unregistration happens in
		// punchedConn.Close()).
		deadline := time.Now().Add(time.Second)
		inUse := func() int {
			mgr.mu.Lock()
			defer mgr.mu.Unlock()
			return len(mgr.conns)
		}
		for inUse() > 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if n := inUse(); n != 0 {
			t.Fatalf("round %d: %d holes left unclosed: hole leak", i, n)
		}
	}
}

func TestLazyConnAddrsBeforeDial(t *testing.T) {
	dialer := &fakeDialer{}
	dialer.cleanup(t)
	mgr, _ := newTestManager(t, dialer)

	conn := newLazyConn(mgr)
	defer conn.Close()
	// Must not panic before the connect.
	_ = conn.LocalAddr()
	_ = conn.RemoteAddr()
}

func newLazyConn(mgr *sessionManager) net.Conn {
	return &lazyConn{
		mgr:      mgr,
		closedCh: make(chan struct{}),
		dialedCh: make(chan struct{}),
	}
}
