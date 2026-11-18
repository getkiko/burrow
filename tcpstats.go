package burrow

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// tcpStatsInterval is the TCP statistics sampling interval. A longer window
// makes throughput and RTT in a UI lag noticeably and averages away bursts;
// one second matches common network tools, and reading TCP_INFO is cheap.
const tcpStatsInterval = 1 * time.Second

var errTCPStatsUnsupported = errors.New("TCP statistics are unsupported on this platform")

type tcpStats struct {
	txBytes          uint64
	retransBytes     uint64
	txPackets        uint64
	retransPackets   uint64
	rtt              time.Duration
	congestionWindow uint64

	// Receive side. rxBytes is the actual throughput; receiveWindow is the
	// local receive window in bytes and receiveSpace the kernel's receive
	// buffer auto-tuning estimate (Linux only). When downloads do not
	// saturate the link, the bottleneck is often the receive window rather
	// than the congestion window.
	rxBytes       uint64
	rxPackets     uint64
	receiveWindow uint64
	receiveSpace  uint64
}

// TCPStats is a snapshot of connection quality for one sampling interval.
//
// Interval fields (TxBytes, RxBytes and so on) are deltas within the
// interval; Total* fields are cumulative since the connection was
// established; RTT, CongestionWindow and Receive* are instantaneous values at
// sampling time.
//
// Derived metrics such as retransmission rate or Mbps are left to the
// consumer.
type TCPStats struct {
	Role       string // "dialer" or "listener"
	Session    string // burrow session ID (unrelated to any multiplexer session)
	LocalAddr  string
	RemoteAddr string
	Interval   time.Duration // length of this sampling interval

	// Deltas within the interval.
	TxBytes        uint64
	RetransBytes   uint64
	TxPackets      uint64
	RetransPackets uint64
	RxBytes        uint64
	RxPackets      uint64

	// Cumulative values.
	TotalTxBytes      uint64
	TotalRetransBytes uint64
	TotalRxBytes      uint64

	// Instantaneous values.
	RTT              time.Duration
	CongestionWindow uint64 // congestion window in bytes
	ReceiveWindow    uint64 // local receive window in bytes; 0 if the kernel does not report it (Linux < 6.2)
	ReceiveSpace     uint64 // receive buffer auto-tuning estimate in bytes (Linux only)
}

// toTCPStats converts two consecutive internal samples into exported
// interval statistics.
func toTCPStats(connectionRole role, session, localAddr, remoteAddr string,
	interval time.Duration, current, previous tcpStats) TCPStats {
	return TCPStats{
		Role:       connectionRole.String(),
		Session:    session,
		LocalAddr:  localAddr,
		RemoteAddr: remoteAddr,
		Interval:   interval,

		TxBytes:        counterDelta(current.txBytes, previous.txBytes),
		RetransBytes:   counterDelta(current.retransBytes, previous.retransBytes),
		TxPackets:      counterDelta(current.txPackets, previous.txPackets),
		RetransPackets: counterDelta(current.retransPackets, previous.retransPackets),
		RxBytes:        counterDelta(current.rxBytes, previous.rxBytes),
		RxPackets:      counterDelta(current.rxPackets, previous.rxPackets),

		TotalTxBytes:      current.txBytes,
		TotalRetransBytes: current.retransBytes,
		TotalRxBytes:      current.rxBytes,

		RTT:              current.rtt,
		CongestionWindow: current.congestionWindow,
		ReceiveWindow:    current.receiveWindow,
		ReceiveSpace:     current.receiveSpace,
	}
}

type syscallConn interface {
	SyscallConn() (syscall.RawConn, error)
}

func startTCPStatsMonitor(logger DebugLogFunc, statsLog func(TCPStats), connectionRole role, session string, conn net.Conn) {
	if conn == nil {
		return
	}
	// Nothing to do without a consumer.
	if logger == nil && statsLog == nil {
		return
	}
	previous, err := readTCPStats(conn)
	if err != nil {
		return
	}
	localAddr := conn.LocalAddr().String()
	remoteAddr := conn.RemoteAddr().String()
	go func() {
		ticker := time.NewTicker(tcpStatsInterval)
		defer ticker.Stop()
		previousAt := time.Now()
		for now := range ticker.C {
			current, err := readTCPStats(conn)
			if err != nil {
				return
			}
			elapsed := now.Sub(previousAt)
			stats := toTCPStats(connectionRole, session, localAddr, remoteAddr, elapsed, current, previous)

			// tcp_stats is logged at debug, not info: one line per interval
			// with over twenty fields would bury the useful lines (punch
			// progress, connections opening and closing) in a UI log view.
			// The structured data already reaches the UI via StatsLog; the
			// line is still visible at debug level for troubleshooting.
			if logger != nil {
				debugLogf(logger, "tcp_stats role=%s session=%s local=%s remote=%s interval=%s "+
					"tx_bytes=%d retrans_bytes=%d retrans_rate=%.3f%% tx_packets=%d retrans_packets=%d packet_retrans_rate=%.3f%% "+
					"rx_bytes=%d rx_packets=%d rx_mbps=%.2f total_rx_bytes=%d "+
					"total_tx_bytes=%d total_retrans_bytes=%d total_retrans_rate=%.3f%% rtt=%s cwnd_bytes=%d rcv_space=%d rcv_wnd=%d",
					connectionRole, shortSession(session), localAddr, remoteAddr, elapsed.Round(time.Millisecond),
					stats.TxBytes, stats.RetransBytes, ratioPercent(stats.RetransBytes, stats.TxBytes),
					stats.TxPackets, stats.RetransPackets, ratioPercent(stats.RetransPackets, stats.TxPackets),
					stats.RxBytes, stats.RxPackets, megabitsPerSecond(stats.RxBytes, elapsed), stats.TotalRxBytes,
					stats.TotalTxBytes, stats.TotalRetransBytes, ratioPercent(stats.TotalRetransBytes, stats.TotalTxBytes),
					stats.RTT.Round(time.Microsecond), stats.CongestionWindow,
					stats.ReceiveSpace, stats.ReceiveWindow)
			}
			if statsLog != nil {
				statsLog(stats)
			}

			previous = current
			previousAt = now
		}
	}()
}

func readTCPStats(conn net.Conn) (tcpStats, error) {
	rawConnProvider, ok := conn.(syscallConn)
	if !ok {
		return tcpStats{}, errTCPStatsUnsupported
	}
	rawConn, err := rawConnProvider.SyscallConn()
	if err != nil {
		return tcpStats{}, err
	}
	var stats tcpStats
	var socketErr error
	err = rawConn.Control(func(fd uintptr) {
		stats, socketErr = readTCPStatsFD(int(fd))
	})
	if err != nil {
		return tcpStats{}, err
	}
	return stats, socketErr
}

func counterDelta(current, previous uint64) uint64 {
	if current >= previous {
		return current - previous
	}
	return current
}

func ratioPercent(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) * 100 / float64(total)
}

// megabitsPerSecond converts bytes over elapsed into Mbps for comparison with
// link bandwidth.
func megabitsPerSecond(bytes uint64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(bytes) * 8 / elapsed.Seconds() / 1e6
}
