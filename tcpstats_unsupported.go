//go:build !darwin && !linux

package burrow

func readTCPStatsFD(int) (tcpStats, error) {
	return tcpStats{}, errTCPStatsUnsupported
}
