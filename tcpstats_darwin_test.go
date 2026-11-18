//go:build darwin

package burrow

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTCPStatsFromConnectionInfoUsesReceiveWindow checks that ReceiveWindow
// is the receive window in bytes, not the window scale factor.
func TestTCPStatsFromConnectionInfoUsesReceiveWindow(t *testing.T) {
	stats := tcpStatsFromConnectionInfo(&unix.TCPConnectionInfo{
		Rcv_wnd:    131072,
		Rcv_wscale: 6,
		Snd_wnd:    1234,
	})
	if stats.receiveWindow != 131072 {
		t.Errorf("receiveWindow = %d, want 131072 (tcpi_rcv_wnd)", stats.receiveWindow)
	}
}

// TestReadTCPStatsReceiveWindowLive reads a real loopback connection and
// checks that the receive window is a plausible byte count.
func TestReadTCPStatsReceiveWindowLive(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()

	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var stats tcpStats
	var readErr error
	if err := raw.Control(func(fd uintptr) { stats, readErr = readTCPStatsFD(int(fd)) }); err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	// A window scale factor is at most 14; a real receive window is far larger.
	if stats.receiveWindow < 1024 {
		t.Fatalf("receiveWindow = %d, want a byte count", stats.receiveWindow)
	}
}
