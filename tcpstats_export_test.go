package burrow

import (
	"testing"
	"time"
)

// TestToTCPStatsConversion checks the conversion from internal tcpStats to
// the exported TCPStats.
func TestToTCPStatsConversion(t *testing.T) {
	current := tcpStats{
		txBytes:          1000,
		retransBytes:     10,
		txPackets:        100,
		retransPackets:   2,
		rtt:              48 * time.Millisecond,
		congestionWindow: 14600,
		rxBytes:          5000,
		rxPackets:        200,
		receiveWindow:    65535,
		receiveSpace:     32768,
	}
	previous := tcpStats{
		txBytes:        400,
		retransBytes:   4,
		txPackets:      40,
		retransPackets: 1,
		rxBytes:        2000,
		rxPackets:      80,
	}

	stats := toTCPStats(roleDialer, "sess-abcdef", "192.168.1.5:41230", "1.2.3.4:54321",
		5*time.Second, current, previous)

	if stats.Role != "dialer" {
		t.Errorf("Role = %q, want dialer", stats.Role)
	}
	if stats.LocalAddr != "192.168.1.5:41230" {
		t.Errorf("LocalAddr = %q", stats.LocalAddr)
	}
	if stats.RemoteAddr != "1.2.3.4:54321" {
		t.Errorf("RemoteAddr = %q", stats.RemoteAddr)
	}
	if stats.Interval != 5*time.Second {
		t.Errorf("Interval = %v", stats.Interval)
	}
	// Interval delta = current - previous.
	if stats.TxBytes != 600 {
		t.Errorf("TxBytes = %d, want 600", stats.TxBytes)
	}
	if stats.RetransBytes != 6 {
		t.Errorf("RetransBytes = %d, want 6", stats.RetransBytes)
	}
	if stats.TxPackets != 60 {
		t.Errorf("TxPackets = %d, want 60", stats.TxPackets)
	}
	if stats.RetransPackets != 1 {
		t.Errorf("RetransPackets = %d, want 1", stats.RetransPackets)
	}
	if stats.RxBytes != 3000 {
		t.Errorf("RxBytes = %d, want 3000", stats.RxBytes)
	}
	if stats.RxPackets != 120 {
		t.Errorf("RxPackets = %d, want 120", stats.RxPackets)
	}
	// Cumulative values are the current absolute values.
	if stats.TotalTxBytes != 1000 {
		t.Errorf("TotalTxBytes = %d, want 1000", stats.TotalTxBytes)
	}
	if stats.TotalRetransBytes != 10 {
		t.Errorf("TotalRetransBytes = %d, want 10", stats.TotalRetransBytes)
	}
	if stats.TotalRxBytes != 5000 {
		t.Errorf("TotalRxBytes = %d, want 5000", stats.TotalRxBytes)
	}
	// Instantaneous values.
	if stats.RTT != 48*time.Millisecond {
		t.Errorf("RTT = %v", stats.RTT)
	}
	if stats.CongestionWindow != 14600 {
		t.Errorf("CongestionWindow = %d", stats.CongestionWindow)
	}
	if stats.ReceiveWindow != 65535 {
		t.Errorf("ReceiveWindow = %d", stats.ReceiveWindow)
	}
	if stats.ReceiveSpace != 32768 {
		t.Errorf("ReceiveSpace = %d", stats.ReceiveSpace)
	}
}

// TestToTCPStatsCounterReset checks that after a counter reset the interval
// value is current itself.
func TestToTCPStatsCounterReset(t *testing.T) {
	current := tcpStats{txBytes: 50}
	previous := tcpStats{txBytes: 1000}

	stats := toTCPStats(roleListener, "s", "l", "r", time.Second, current, previous)

	if stats.Role != "listener" {
		t.Errorf("Role = %q, want listener", stats.Role)
	}
	// counterDelta returns current when current < previous.
	if stats.TxBytes != 50 {
		t.Errorf("TxBytes = %d, want 50 (counter reset)", stats.TxBytes)
	}
}
