//go:build linux

package burrow

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestTCPStatsFromTCPInfoUsesReceiveWindow checks that ReceiveWindow comes
// from the local receive window, not the peer's advertised (send) window.
func TestTCPStatsFromTCPInfoUsesReceiveWindow(t *testing.T) {
	stats := tcpStatsFromTCPInfo(&unix.TCPInfo{
		Rcv_wnd:   65536,
		Snd_wnd:   1234,
		Rcv_space: 14600,
		Snd_cwnd:  10,
		Snd_mss:   1448,
	})
	if stats.receiveWindow != 65536 {
		t.Errorf("receiveWindow = %d, want 65536 (tcpi_rcv_wnd)", stats.receiveWindow)
	}
	if stats.receiveSpace != 14600 {
		t.Errorf("receiveSpace = %d, want 14600", stats.receiveSpace)
	}
	if stats.congestionWindow != 14480 {
		t.Errorf("congestionWindow = %d, want 14480", stats.congestionWindow)
	}
}
