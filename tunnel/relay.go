package tunnel

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// countingWriter reports the byte count after every write.
//
// Reporting per write, rather than adding up once io.Copy returns, matters
// because io.Copy only returns when the connection ends. A long connection
// (browsing, video) can last minutes, during which the UI would show zero
// traffic and speed and then jump when it closes.
type countingWriter struct {
	w      io.Writer
	report func(uint64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 && c.report != nil {
		c.report(uint64(n))
	}
	return n, err
}

// relayCounted copies bytes between a and b in both directions until either
// direction ends, and returns the byte count of each direction. It never
// parses the data: that is the tunnel's core contract, with addressing left
// to the upper-layer protocol.
//
// When one direction ends both ends are closed so the other direction's
// blocked read returns promptly. Data still in flight is dropped, which is
// the right trade-off here: the upper protocol has its own connection
// semantics, and a half-closed state means nothing to it.
//
// Byte counts are reported while data flows: onA receives increments for the
// a->b direction, onB for b->a.
func relayCounted(a, b io.ReadWriteCloser, onA, onB func(uint64)) (fromA, fromB int64) {
	var wg sync.WaitGroup

	wg.Go(func() {
		fromA, _ = io.Copy(&countingWriter{w: b, report: onA}, a)
		a.Close()
		b.Close()
	})
	wg.Go(func() {
		fromB, _ = io.Copy(&countingWriter{w: a, report: onB}, b)
		a.Close()
		b.Close()
	})

	wg.Wait()
	return fromA, fromB
}

// closeNotifier is an optional interface. A stream that implements it exposes
// a channel signalling "closed or about to become unusable" without having to
// be read.
//
// While the conn->stream direction of relayDeferReverse has written nothing,
// it must not read the stream to detect a dead tunnel (that is exactly what
// deferred punching forbids). Without some other signal, the conn->stream
// direction would block in conn.Read forever and leak a goroutine and a
// socket. closeNotifier is that signal; the production lazyConn implements it
// (see session.go). A stream without it (such as a bare net.Conn in tests)
// can only finish when conn itself closes, an inherent limit of the generic
// type.
type closeNotifier interface {
	// CloseNotify returns a channel that is closed once the stream is closed
	// or known to be unusable. Repeated calls should return the same or an
	// equivalent channel.
	CloseNotify() <-chan struct{}
}

// relayDeferReverse copies between conn and stream in both directions, but
// starts the stream->conn direction only after conn->stream has actually
// written its first bytes.
//
// Originally the stream was a lazyConn that punched on its first Read or
// Write, and the delay kept io.Copy(conn, stream) from reading the stream
// immediately and punching early. Holes are now punched ahead of time (the
// pre-punch pool) and taken in Client.handle, so the delay no longer serves
// that purpose.
//
// Holes are never recycled here. An earlier version used "nothing written
// yet" as a sign that the hole was clean and returned it to the pool when the
// forward copy ended. That is wrong: with a server-speaks-first protocol the
// forward conn.Read sees no data for a while, so a connection in active use
// was judged unused and its hole handed back, and clients were disconnected
// right after connecting. More fundamentally, once taken from the pool a hole
// belongs to that connection: the server dials the forward target as soon as
// it reads the first byte, and reusing the hole would put a new request into
// the old forwarding context. Taken means dedicated.
//
// The function remains because it avoids a deadlock. An earlier design read
// one byte synchronously before relaying, which stalls server-speaks-first
// protocols: the client waits for the peer and the peer waits for the
// server's greeting. Here only the stream->conn direction is deferred, with no
// blocking wait:
//   - client speaks first: the first conn->stream Write starts the reverse
//     direction;
//   - server speaks first: a quiet conn->stream direction blocks nothing; the
//     reverse direction simply has not started, and closing conn finishes
//     both directions normally.
//
// If conn->stream ends without ever writing (a client connects and leaves
// without sending), the stream is never read and never triggers punching, as
// intended: no real data, no punching cost. The reverse direction is
// therefore started only after at least one byte was written; otherwise it
// does not io.Copy(conn, stream) and read the stream for nothing.
//
// Not reading the stream does not mean ignoring its closure. If the stream
// implements closeNotifier, its CloseNotify() is watched before the trigger;
// when the stream side goes away (the tunnel dropped, or a fanout candidate
// was judged a zombie and closed), conn and stream are closed and the
// function returns, unblocking the conn->stream Read. (An earlier version
// returned from that branch without closing anything, leaving conn->stream
// blocked forever and leaking the goroutine and socket.) Without
// closeNotifier the function can only finish when conn closes.
//
// onConn receives increments for conn->stream (upload) and onStream for
// stream->conn (download); either may be nil. The third result, used,
// reports whether conn->stream actually wrote any bytes.
func relayDeferReverse(
	conn, stream io.ReadWriteCloser,
	onConn, onStream func(uint64),
) (fromConn, fromStream int64, used bool) {
	var wg sync.WaitGroup
	reverseStarted := make(chan struct{})
	var triggered atomic.Bool

	wg.Go(func() {
		fromConn, _ = copyAndSignalFirstWrite(
			&countingWriter{w: stream, report: onConn}, conn, reverseStarted, &triggered)
		conn.Close()
		stream.Close()
	})
	wg.Go(func() {
		var streamClosed <-chan struct{}
		if notifier, ok := stream.(closeNotifier); ok {
			streamClosed = notifier.CloseNotify()
		}
		select {
		case <-reverseStarted:
			if !triggered.Load() {
				// The forward direction ended without writing anything (the
				// channel was closed on exit, not by a trigger). The stream
				// must not be read at all, but both ends are still closed:
				// the forward goroutine already did so, and this idempotent
				// close covers a stream that dropped before the forward
				// direction noticed.
				conn.Close()
				stream.Close()
				return
			}
			fromStream, _ = io.Copy(&countingWriter{w: conn, report: onStream}, stream)
			conn.Close()
			stream.Close()
		case <-streamClosed:
			// The stream side went away before the trigger (tunnel dropped,
			// zombie candidate cleaned up). The stream must not be read to
			// confirm it, but conn must be closed so the blocked
			// conn->stream Read returns; otherwise it hangs forever.
			conn.Close()
			stream.Close()
		}
	})

	wg.Wait()
	return fromConn, fromStream, triggered.Load()
}

// copyAndSignalFirstWrite copies from src to dst. On the first successful
// write it sets triggered and closes signal so the reverse direction starts.
// If the copy ends without writing anything, it only closes signal to release
// the waiter and leaves triggered false, telling the caller not to start the
// reverse direction.
func copyAndSignalFirstWrite(dst io.Writer, src io.Reader, signal chan struct{}, triggered *atomic.Bool) (int64, error) {
	var signalOnce sync.Once
	closeSignal := func() { signalOnce.Do(func() { close(signal) }) }
	defer closeSignal()

	buf := make([]byte, 32*1024)
	var written int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			wn, writeErr := dst.Write(buf[:n])
			written += int64(wn)
			if wn > 0 {
				triggered.Store(true)
				closeSignal()
			}
			if writeErr != nil {
				return written, writeErr
			}
			if wn != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

// isTimeoutError reports whether err is an I/O timeout (deadline exceeded).
//
// It separates "the peer closed the connection", a definite signal, from
// "waiting timed out", which suggests the peer vanished without sending a
// FIN or RST.
func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// isNormalClose reports whether err is an ordinary connection shutdown that
// does not need logging.
func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}
