package tunnel

import (
	"net"
	"testing"
	"time"
)

// TestVerifyCloseRaceWithPendingRegistration covers the race where Close() is
// called after a conn is accepted but before registerConn has registered it.
// Close() must still close that conn so wg.Wait() does not block forever.
//
// Unlike TestVerifyServerCloseDoesNotHang, which sleeps 200ms to let
// registration finish, this test does not wait, aiming at the narrow window
// where Accept returning and Close() interleave; 50 rounds raise the odds of
// hitting it.
func TestVerifyCloseRaceWithPendingRegistration(t *testing.T) {
	for round := range 50 {
		incoming := make(chan net.Conn, 1)
		listener := &pipeListener{incoming: incoming, closed: make(chan struct{})}

		server := &Server{
			forward:  "127.0.0.1:1",
			listener: listener,
			tracker:  newStatusTracker("r", nil),
			logger:   newLogger("error"),
			// Zombie timeout far above the test timeout, so Close() must
			// return by closing activeConns, not by the deadline expiring.
			zombieTimeout: time.Hour,
			done:          make(chan struct{}),
		}
		if err := server.Start(); err != nil {
			t.Fatalf("round %d: start server: %v", round, err)
		}

		// A peer that stays connected and silent is enough; no protocol needed.
		serverSide, clientSide := net.Pipe()

		// Close right after feeding the conn, before acceptLoop can register it.
		incoming <- serverSide
		done := make(chan struct{})
		go func() {
			server.Close()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			clientSide.Close()
			t.Fatalf("round %d: Close() did not return within 3s: "+
				"conn was accepted but not yet in activeConns and Range missed it", round)
		}
		clientSide.Close()
	}
}
