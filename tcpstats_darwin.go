//go:build darwin

package burrow

import (
	"time"

	"golang.org/x/sys/unix"
)

func readTCPStatsFD(fd int) (tcpStats, error) {
	info, err := unix.GetsockoptTCPConnectionInfo(fd, unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	if err != nil {
		return tcpStats{}, err
	}
	return tcpStatsFromConnectionInfo(info), nil
}

// tcpStatsFromConnectionInfo maps the XNU tcp_connection_info structure.
// tcpi_rcv_wnd is the local receive window in bytes (tp->rcv_wnd); XNU does
// not expose a receive-buffer auto-tuning estimate, so receiveSpace stays 0.
func tcpStatsFromConnectionInfo(info *unix.TCPConnectionInfo) tcpStats {
	return tcpStats{
		txBytes:          info.Txbytes,
		retransBytes:     info.Txretransmitbytes,
		txPackets:        info.Txpackets,
		retransPackets:   info.Txretransmitpackets,
		rtt:              time.Duration(info.Rttcur) * time.Millisecond,
		congestionWindow: uint64(info.Snd_cwnd),
		rxBytes:          info.Rxbytes,
		rxPackets:        info.Rxpackets,
		receiveWindow:    uint64(info.Rcv_wnd),
	}
}
