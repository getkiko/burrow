package tunnel

import (
	"net"
	"testing"
	"time"
)

// TestClientCloseRaceWithPendingRegistration mirrors the server-side
// TestVerifyCloseRaceWithPendingRegistration. It covers the window where a
// local TCP connection has just been accepted by acceptLoop but not yet
// registered via registerConn when Close is called: Close must still close
// that connection reliably instead of leaving wg.Wait() hanging.
//
// The client's handle() blocks immediately in conn.Read inside
// relayDeferReverse(conn, stream) (no hole yet, and the upper protocol has
// said nothing). If both registerConn and Close's Range missed the conn in
// that window, nothing would ever close it.
func TestClientCloseRaceWithPendingRegistration(t *testing.T) {
	for round := range 50 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("round %d: listen: %v", round, err)
		}

		tracker := newStatusTracker("test-room", nil)
		logger := newLogger("error")
		client := &Client{
			listener: listener,
			tracker:  tracker,
			logger:   logger,
			// A dialer that never yields a connection: no hole is needed,
			// only that Close still finishes inside the race window.
			sessions: newSessionManager(&pipeDialer{incoming: make(chan net.Conn)}, tracker, logger, 0),
			done:     make(chan struct{}),
		}
		if err := client.Start(); err != nil {
			t.Fatalf("round %d: start client: %v", round, err)
		}

		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatalf("round %d: dial: %v", round, err)
		}

		// Close right after connecting, giving registerConn in acceptLoop no
		// time to finish, to hit the window.
		done := make(chan struct{})
		go func() {
			client.Close()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			conn.Close()
			t.Fatalf("round %d: Close() did not return within 3s; race confirmed: "+
				"conn was accepted but not yet in activeConns, and Range missed it", round)
		}
		conn.Close()
	}
}
