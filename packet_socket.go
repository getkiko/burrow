package burrow

import (
	"context"
	"fmt"
	"net"
)

// listenUDP binds a UDP socket on bindAddr (an IP without a port) with a
// kernel-assigned port. socketControl layers SO_REUSEADDR/SO_REUSEPORT and
// the host's own cfg.Control callback (for example binding to a physical
// interface), matching allocLocalPort on the TCP path.
func listenUDP(ctx context.Context, bindAddr string, cfg PacketConfig) (*net.UDPConn, error) {
	lc := &net.ListenConfig{Control: socketControl(cfg.Control)}
	pc, err := lc.ListenPacket(ctx, "udp4", bindAddr+":0")
	if err != nil {
		return nil, fmt.Errorf("burrow: bind local UDP socket: %w", err)
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("burrow: internal error: ListenPacket(\"udp4\") did not return *net.UDPConn")
	}
	return conn, nil
}

// udpSocketBindAddr picks the local IP for UDP sockets by reusing bindIP
// from the TCP path: the same "prefer the physical interface, otherwise ask
// the kernel's routing" logic applies to UDP. The routing probe is a
// one-off TCP dial used only to learn which source address the kernel
// would choose; it does not mean TCP is used afterwards.
func udpSocketBindAddr(ctx context.Context, cfg PacketConfig) (string, error) {
	return bindIP(ctx, cfg.interfaceName(), cfg.SignalAddr, "tcp4", cfg.Control, cfg.ResolveIP, cfg.InterfaceAddr, cfg.DebugLog)
}
