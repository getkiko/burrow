package tunnel

import (
	"net"
	"testing"
	"time"
)

// TestVerifyServerCloseDoesNotHang checks that Server.Close() does not hang
// when a candidate has been accepted and the peer never disconnects.
func TestVerifyServerCloseDoesNotHang(t *testing.T) {
	incoming := make(chan net.Conn, 1)
	listener := &pipeListener{incoming: incoming, closed: make(chan struct{})}

	server := &Server{
		forward:  "127.0.0.1:1",
		listener: listener,
		tracker:  newStatusTracker("r", nil),
		logger:   newLogger("error"),
		// Zombie timeout far above the test timeout, so Close() cannot pass
		// merely because the first-byte deadline happened to expire; the
		// test must exercise Close() actively closing the candidate.
		zombieTimeout: time.Hour,
		done:          make(chan struct{}),
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// Feed one candidate whose peer stays connected (an open hole). Keeping
	// the conn open is enough to block the server on the first-byte read,
	// which is the hang this test covers.
	serverSide, clientSide := net.Pipe()
	incoming <- serverSide
	defer clientSide.Close()

	// Let the server accept and block on the first-byte read.
	time.Sleep(200 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		server.Close()
		close(done)
	}()

	select {
	case <-done:
		// Close returned.
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Close() did not return within 3s: " +
			"serveConn is still blocked on the first-byte read and wg.Wait() never finishes; " +
			"in production the CLI would hang on SIGTERM until SIGKILL")
	}
}

// TestServerCloseDoesNotWaitForZombieTimeout checks that Close() does not
// wait out zombieTimeout for a zombie candidate.
//
// A zombie candidate never sends a byte, so serveConn stays blocked on the
// first-byte read until zombieTimeout expires. zombieTimeout is set far above
// the test timeout to prove Close() actively closes activeConns rather than
// riding on the timeout.
func TestServerCloseDoesNotWaitForZombieTimeout(t *testing.T) {
	incoming := make(chan net.Conn, 1)
	listener := &pipeListener{incoming: incoming, closed: make(chan struct{})}

	server := &Server{
		forward:       "127.0.0.1:1",
		listener:      listener,
		tracker:       newStatusTracker("r", nil),
		logger:        newLogger("error"),
		zombieTimeout: time.Hour, // far above the test timeout
		done:          make(chan struct{}),
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// Feed a zombie candidate: the peer neither sends nor closes, so the
	// server blocks on the first-byte read forever.
	serverSide, clientSide := net.Pipe()
	incoming <- serverSide
	defer clientSide.Close()

	// Let the server accept and block on the first-byte read.
	time.Sleep(200 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		server.Close()
		close(done)
	}()

	select {
	case <-done:
		// Close returned without waiting for zombieTimeout.
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Close() did not return within 3s: a zombie candidate is " +
			"still blocked on the first-byte read and Close() did not close activeConns")
	}
}
