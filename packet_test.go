package burrow

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func newLoopbackUDPConn(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func isUDPConnClosed(conn *net.UDPConn) bool {
	// SetReadDeadline fails immediately on a closed socket and always
	// succeeds on an open one, which detects the closed state without
	// depending on error text.
	return conn.SetReadDeadline(time.Now()) != nil
}

// Commit must keep the winning socket and close all other sockets.
func TestPacketAttemptCommitClosesLosers(t *testing.T) {
	winner := newLoopbackUDPConn(t)
	loser1 := newLoopbackUDPConn(t)
	loser2 := newLoopbackUDPConn(t)
	defer winner.Close()

	attempt := &PacketAttempt{
		Session: "s1",
		Paths: []PacketPath{
			{Conn: loser1},
			{Conn: winner},
			{Conn: loser2},
		},
	}

	got, err := attempt.Commit(1, netip.MustParseAddrPort("203.0.113.1:1"))
	if err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	if got != winner {
		t.Fatalf("Commit returned a socket other than the winner")
	}
	if !isUDPConnClosed(loser1) {
		t.Error("loser1 should have been closed by Commit")
	}
	if !isUDPConnClosed(loser2) {
		t.Error("loser2 should have been closed by Commit")
	}
	if isUDPConnClosed(winner) {
		t.Error("winner must not be closed by Commit")
	}
}

// Commit must reject an out-of-range path index.
func TestPacketAttemptCommitOutOfRange(t *testing.T) {
	conn := newLoopbackUDPConn(t)
	defer conn.Close()
	attempt := &PacketAttempt{Paths: []PacketPath{{Conn: conn}}}
	if _, err := attempt.Commit(5, netip.AddrPort{}); err == nil {
		t.Fatal("expected an error for an out-of-range index")
	}
}

// Commit may be called only once.
func TestPacketAttemptCommitOnlyOnce(t *testing.T) {
	conn1 := newLoopbackUDPConn(t)
	conn2 := newLoopbackUDPConn(t)
	defer conn1.Close()
	attempt := &PacketAttempt{Paths: []PacketPath{{Conn: conn1}, {Conn: conn2}}}
	if _, err := attempt.Commit(0, netip.AddrPort{}); err != nil {
		t.Fatalf("first Commit failed: %v", err)
	}
	if _, err := attempt.Commit(0, netip.AddrPort{}); err == nil {
		t.Fatal("expected an error for a second Commit")
	}
}

// Close must release every socket and be safe to call repeatedly.
func TestPacketAttemptCloseReleasesAll(t *testing.T) {
	conn1 := newLoopbackUDPConn(t)
	conn2 := newLoopbackUDPConn(t)
	attempt := &PacketAttempt{Paths: []PacketPath{{Conn: conn1}, {Conn: conn2}}}

	if err := attempt.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !isUDPConnClosed(conn1) || !isUDPConnClosed(conn2) {
		t.Fatal("all sockets should be closed after Close")
	}
	if err := attempt.Close(); err != nil {
		t.Fatalf("second Close should be safe: %v", err)
	}
}

// Commit after Close must return ErrClosed.
func TestPacketAttemptCommitAfterClose(t *testing.T) {
	conn := newLoopbackUDPConn(t)
	attempt := &PacketAttempt{Paths: []PacketPath{{Conn: conn}}}
	_ = attempt.Close()
	if _, err := attempt.Commit(0, netip.AddrPort{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Commit after Close: want ErrClosed, got %v", err)
	}
}

// Close after Commit must also close the winning socket. The caller must
// eventually call Close to avoid leaks; Attempt.Close cannot know whether
// the caller has taken over the winner, so the caller decides when.
func TestPacketAttemptCloseAfterCommit(t *testing.T) {
	winner := newLoopbackUDPConn(t)
	loser := newLoopbackUDPConn(t)
	attempt := &PacketAttempt{Paths: []PacketPath{{Conn: loser}, {Conn: winner}}}

	if _, err := attempt.Commit(1, netip.AddrPort{}); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}
	if err := attempt.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !isUDPConnClosed(winner) {
		t.Error("Close should also close the socket kept by Commit")
	}
}
