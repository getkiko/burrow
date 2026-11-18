package burrow

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

// mockSTUNServer is a STUN Binding server on a real UDP socket. It answers
// each request with a Binding Success response carrying XOR-MAPPED-ADDRESS.
// With a nil respondWith it echoes the request's source address, which is
// what a real STUN server reports on loopback where there is no NAT. With a
// non-nil respondWith it always returns that address, which helps assert
// cases such as "one socket sees the same mapping for different targets".
// It keeps the tests independent of public STUN servers.
type mockSTUNServer struct {
	conn *net.UDPConn
	addr string
	done chan struct{}
}

func startMockSTUNServer(t *testing.T, respondWith *net.UDPAddr) *mockSTUNServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("start mock STUN server: %v", err)
	}
	s := &mockSTUNServer{conn: conn, addr: conn.LocalAddr().String(), done: make(chan struct{})}
	go s.serve(respondWith)
	t.Cleanup(func() { s.Close() })
	return s
}

// startEchoSTUNServer starts a mock STUN server in echo mode: the mapped
// address is the caller socket's actual source address.
func startEchoSTUNServer(t *testing.T) *mockSTUNServer {
	t.Helper()
	return startMockSTUNServer(t, nil)
}

func (s *mockSTUNServer) serve(respondWith *net.UDPAddr) {
	defer close(s.done)
	buf := make([]byte, 1500)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if isClosedConnErr(err) {
				return
			}
			continue
		}
		request := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if err := request.Decode(); err != nil {
			continue
		}
		response := stun.MustBuild(request, stun.BindingSuccess)
		mapped := respondWith
		if mapped == nil {
			mapped = from
		}
		xorAddr := stun.XORMappedAddress{IP: mapped.IP, Port: mapped.Port}
		if err := xorAddr.AddTo(response); err != nil {
			continue
		}
		response.Encode()
		_, _ = s.conn.WriteToUDP(response.Raw, from)
	}
}

// isClosedConnErr tells a read timeout (keep looping) from a closed
// connection (stop serving).
func isClosedConnErr(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	return true
}

func (s *mockSTUNServer) Close() {
	_ = s.conn.Close()
	<-s.done
}
