package burrow

import (
	"net"
	"testing"
	"time"
)

// queryUDPMappedAddr returns the mapped address from a real STUN response.
func TestQueryUDPMappedAddrSuccess(t *testing.T) {
	want := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 42), Port: 51820}
	server := startMockSTUNServer(t, want)

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	got, err := queryUDPMappedAddr(conn, server.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("queryUDPMappedAddr: %v", err)
	}
	if !got.IP.Equal(want.IP) || got.Port != want.Port {
		t.Fatalf("mapped address mismatch: got %v, want %v", got, want)
	}
}

// queryUDPMappedAddr against an unreachable reserved address (TEST-NET-1,
// RFC 5737) must fail with a timeout in reasonable time instead of hanging.
// Enforcing the timeout with time.AfterFunc(timeout, client.Close) made
// client.Do block forever. The test asserts that an error is returned and
// that it does not take much longer than the requested timeout.
func TestQueryUDPMappedAddrTimeout(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const timeout = 1200 * time.Millisecond
	start := time.Now()
	_, err = queryUDPMappedAddr(conn, "192.0.2.1:3478", timeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	t.Logf("timeout error: %v (took %s)", err, elapsed)
	// Allow some slack (the RTO derivation is not exact), but no more than
	// three times the timeout, and certainly no multi-minute hang.
	if elapsed > 3*timeout {
		t.Fatalf("took %s, far beyond %s; busy loop or hang regression?", elapsed, timeout)
	}
}

// The exported wrapper behaves like the internal implementation.
func TestQueryUDPMappedAddrExportedWrapper(t *testing.T) {
	want := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 7), Port: 4000}
	server := startMockSTUNServer(t, want)

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	got, err := QueryUDPMappedAddr(conn, server.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("QueryUDPMappedAddr: %v", err)
	}
	if !got.IP.Equal(want.IP) || got.Port != want.Port {
		t.Fatalf("mapped address mismatch: got %v, want %v", got, want)
	}
}
