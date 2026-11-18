package tunnel

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// relay is relayCounted without byte reporting; only tests use it.
func relay(a, b io.ReadWriteCloser) (fromA, fromB int64) {
	var wg sync.WaitGroup

	wg.Go(func() {
		fromA, _ = io.Copy(b, a)
		a.Close()
		b.Close()
	})
	wg.Go(func() {
		fromB, _ = io.Copy(a, b)
		a.Close()
		b.Close()
	})

	wg.Wait()
	return fromA, fromB
}

func TestRelayBidirectional(t *testing.T) {
	clientSide, tunnelSideA := net.Pipe()
	tunnelSideB, targetSide := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		relay(tunnelSideA, tunnelSideB)
	}()

	clientSide.SetDeadline(time.Now().Add(2 * time.Second))
	targetSide.SetDeadline(time.Now().Add(2 * time.Second))

	// client -> target
	go clientSide.Write([]byte("hello, target"))
	received := make([]byte, len("hello, target"))
	if _, err := io.ReadFull(targetSide, received); err != nil {
		t.Fatalf("target read: %v", err)
	}
	if string(received) != "hello, target" {
		t.Errorf("target received %q", received)
	}

	// target -> client
	go targetSide.Write([]byte("hello, client"))
	back := make([]byte, len("hello, client"))
	if _, err := io.ReadFull(clientSide, back); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(back) != "hello, client" {
		t.Errorf("client received %q", back)
	}

	clientSide.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not exit after one side closed")
	}
	targetSide.Close()
}

func TestRelayReturnsByteCounts(t *testing.T) {
	// Real TCP instead of net.Pipe: Pipe is unbuffered, which makes large
	// transfers awkward to synchronize.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	payload := make([]byte, 64*1024)
	rand.Read(payload)

	// Echo server: read the whole payload, then write it back.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		conn.Write(buf)
	}()

	upstream, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer upstream.Close()

	clientSide, tunnelSide := net.Pipe()
	counts := make(chan [2]int64, 1)
	go func() {
		fromA, fromB := relay(tunnelSide, upstream)
		counts <- [2]int64{fromA, fromB}
	}()

	clientSide.SetDeadline(time.Now().Add(5 * time.Second))
	go func() {
		clientSide.Write(payload)
	}()

	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(clientSide, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(payload, echoed) {
		t.Error("echoed data differs from the original")
	}

	clientSide.Close()
	select {
	case got := <-counts:
		if got[0] != int64(len(payload)) {
			t.Errorf("fromA = %d, want %d", got[0], len(payload))
		}
		if got[1] != int64(len(payload)) {
			t.Errorf("fromB = %d, want %d", got[1], len(payload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not return")
	}
	<-serverDone
}

// probeReader wraps an io.Reader and counts Read calls, to assert that the
// stream is never read before the trigger.
type probeReader struct {
	io.Reader
	calls *atomic.Int32
}

func (p probeReader) Read(b []byte) (int, error) {
	p.calls.Add(1)
	return p.Reader.Read(b)
}

// relayHalf assembles an io.ReadWriteCloser from parts, so a probeReader can
// be wrapped in before handing it to relayDeferReverse.
type relayHalf struct {
	io.Reader
	io.Writer
	io.Closer
}

// notifyingRelayHalf adds closeNotifier to relayHalf, simulating a stream-side
// disconnect (tunnel dropped, zombie candidate cleaned up by the server) that
// is observable without a Read. The test closes closeCh when the event
// happens.
type notifyingRelayHalf struct {
	relayHalf
	closeCh chan struct{}
}

func (n notifyingRelayHalf) CloseNotify() <-chan struct{} { return n.closeCh }

// With fanout, stream->conn must not start until conn->stream has written
// its first bytes; otherwise deferred punching is defeated (the stream is a
// lazyConn and punches on its first read).
func TestRelayDeferReverseWaitsForFirstWrite(t *testing.T) {
	connSide, connPeer := net.Pipe()
	streamSide, streamPeer := net.Pipe()

	var streamReadCalls atomic.Int32
	wrappedStream := relayHalf{
		Reader: probeReader{Reader: streamSide, calls: &streamReadCalls},
		Writer: streamSide,
		Closer: streamSide,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		relayDeferReverse(connPeer, wrappedStream, nil, nil)
	}()

	// Connected but nothing written: the stream must not be read.
	time.Sleep(100 * time.Millisecond)
	if calls := streamReadCalls.Load(); calls != 0 {
		t.Errorf("stream read %d times before conn wrote, want 0", calls)
	}

	// A target (streamPeer) that speaks first must not deadlock. Nothing is
	// sent here; the assertion above already shows that no goroutine is
	// stuck reading the stream.

	// The first bytes from conn start the stream direction.
	connSide.SetDeadline(time.Now().Add(2 * time.Second))
	streamPeer.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := connSide.Write([]byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 2)
	if _, err := io.ReadFull(streamPeer, got); err != nil {
		t.Fatalf("stream side received nothing: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("stream side received %q, want hi", got)
	}

	// After the trigger, the reverse direction (stream->conn) works too.
	if _, err := streamPeer.Write([]byte("ok")); err != nil {
		t.Fatalf("write: %v", err)
	}
	back := make([]byte, 2)
	if _, err := io.ReadFull(connSide, back); err != nil {
		t.Fatalf("conn side received no reverse data: %v", err)
	}
	if string(back) != "ok" {
		t.Errorf("conn side received %q, want ok", back)
	}

	connSide.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relayDeferReverse did not exit after close")
	}
	streamPeer.Close()
}

// This test covers only the path where conn closes itself. The path where
// the stream drops first while conn never writes or closes is covered by
// TestRelayDeferReverseStreamClosesFirstBeforeTrigger; it once hung forever
// (conn->stream blocked in conn.Read with nothing closing conn).
func TestRelayDeferReverseConnClosesBeforeTriggerNeverReadsStream(t *testing.T) {
	connSide, connPeer := net.Pipe()
	streamSide, streamPeer := net.Pipe()
	defer streamPeer.Close()

	var streamReadCalls atomic.Int32
	wrappedStream := relayHalf{
		Reader: probeReader{Reader: streamSide, calls: &streamReadCalls},
		Writer: streamSide,
		Closer: streamSide,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		relayDeferReverse(connPeer, wrappedStream, nil, nil)
	}()

	// Simulate a server-speaks-first protocol: conn only reads, never writes.
	connSide.SetDeadline(time.Now().Add(2 * time.Second))
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		connSide.Read(make([]byte, 1))
	}()

	time.Sleep(100 * time.Millisecond)
	connSide.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relayDeferReverse deadlocked: did not exit while conn only read")
	}
	<-readDone

	if calls := streamReadCalls.Load(); calls != 0 {
		t.Errorf("stream read %d times although conn never wrote, want 0 (an unnecessary punch was triggered)", calls)
	}
}

// Regression test: conn->stream has written nothing, and conn neither writes
// nor closes (a server-speaks-first protocol where the client waits for a
// greeting). If the stream side drops first (tunnel down, zombie candidate
// cleaned up), relayDeferReverse must notice, close conn and return instead
// of blocking in conn.Read forever.
//
// An earlier implementation's reverse goroutine returned on !triggered
// without closing anything or watching the stream, so the forward goroutine
// blocked in conn.Read, wg.Wait() never returned, and every such connection
// leaked a goroutine and a socket. notifyingRelayHalf simulates the
// disconnect signal (in production, the hole-death notice merged into
// lazyConn.CloseNotify(); see punchedConn.died in session.go). The test
// asserts that relayDeferReverse returns promptly, conn is closed, and the
// stream was never read (deferred-punching semantics are preserved).
func TestRelayDeferReverseStreamClosesFirstBeforeTrigger(t *testing.T) {
	connSide, connPeer := net.Pipe()
	streamSide, _ := net.Pipe()
	defer streamSide.Close()

	var streamReadCalls atomic.Int32
	streamCloseNotify := make(chan struct{})
	wrappedStream := notifyingRelayHalf{
		relayHalf: relayHalf{
			Reader: probeReader{Reader: streamSide, calls: &streamReadCalls},
			Writer: streamSide,
			Closer: streamSide,
		},
		closeCh: streamCloseNotify,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		relayDeferReverse(connPeer, wrappedStream, nil, nil)
	}()

	// conn neither writes nor closes (server speaks first); the only event is
	// the stream-side disconnect.
	time.Sleep(50 * time.Millisecond)
	close(streamCloseNotify)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relayDeferReverse deadlocked: did not return after the stream side dropped")
	}

	// conn must be closed so the blocked conn->stream Read is released.
	if _, err := connSide.Write([]byte("x")); err == nil {
		t.Error("conn should be closed after the stream side dropped, but it is still writable")
	}

	if calls := streamReadCalls.Load(); calls != 0 {
		t.Errorf("stream read %d times after dropping before the trigger, want 0 (deferred punching must be preserved)", calls)
	}
}

func TestIsNormalClose(t *testing.T) {
	if !isNormalClose(nil) {
		t.Error("nil should count as a normal close")
	}
	if !isNormalClose(io.EOF) {
		t.Error("io.EOF should count as a normal close")
	}
	if !isNormalClose(net.ErrClosed) {
		t.Error("net.ErrClosed should count as a normal close")
	}
	if !isNormalClose(io.ErrClosedPipe) {
		t.Error("io.ErrClosedPipe should count as a normal close")
	}
	if isNormalClose(io.ErrUnexpectedEOF) {
		t.Error("io.ErrUnexpectedEOF should not count as a normal close")
	}
}
