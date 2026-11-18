//go:build linux

package burrow

import (
	"time"

	"golang.org/x/sys/unix"
)

func readTCPStatsFD(fd int) (tcpStats, error) {
	info, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return tcpStats{}, err
	}
	return tcpStatsFromTCPInfo(info), nil
}

// tcpStatsFromTCPInfo maps the Linux tcp_info structure.
// tcpi_rcv_wnd is the local receive window in bytes (tp->rcv_wnd). It was
// added in Linux 6.2; older kernels return a shorter structure, the field
// stays zero and ReceiveWindow is reported as 0 (unknown).
func tcpStatsFromTCPInfo(info *unix.TCPInfo) tcpStats {
	return tcpStats{
		txBytes:          info.Bytes_sent,
		retransBytes:     info.Bytes_retrans,
		txPackets:        uint64(info.Segs_out),
		retransPackets:   uint64(info.Total_retrans),
		rtt:              time.Duration(info.Rtt) * time.Microsecond,
		congestionWindow: uint64(info.Snd_cwnd) * uint64(info.Snd_mss),
		rxBytes:          info.Bytes_received,
		rxPackets:        uint64(info.Segs_in),
		receiveWindow:    uint64(info.Rcv_wnd),
		receiveSpace:     uint64(info.Rcv_space),
	}
}
