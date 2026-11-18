// Package burrow establishes direct TCP connections between two hosts behind
// NAT using TCP hole punching, coordinated through a signal server.
//
// The API mirrors the standard library. [Listen] returns a [net.Listener]
// whose Accept yields a new [net.Conn] for every peer that dials in, and
// [NewDialer] returns a [Dialer] whose Dial opens a new [net.Conn] to the
// listening peer. What runs over the connection is entirely up to the caller.
//
// Both sides need the same signal server address ([Config.SignalAddr]) and
// the same room name ([Config.Room]). The signal server provides WebSocket
// signaling and reflects each side's public TCP endpoint; it never relays
// application data. A typical setup:
//
//	// One side listens.
//	ln, err := burrow.Listen(burrow.Config{
//		SignalAddr: "signal.example.com:9960",
//		Room:       "my-room",
//	})
//	if err != nil {
//		return err
//	}
//	defer ln.Close()
//	for {
//		conn, err := ln.Accept()
//		if err != nil {
//			return err
//		}
//		go handle(conn)
//	}
//
//	// The other side dials.
//	d, err := burrow.NewDialer(burrow.Config{
//		SignalAddr: "signal.example.com:9960",
//		Room:       "my-room",
//	})
//	if err != nil {
//		return err
//	}
//	defer d.Close()
//	conn, err := d.Dial(context.Background())
//
// # Strategies
//
// [Config.Strategy] selects how the two sides punch. The Dialer decides the
// strategy parameters and sends them to the Listener in the DIAL request.
//
//   - [StrategyDirect]: each side binds one local port, learns its public
//     endpoint from the signal server, and both sides dial each other's
//     endpoint while also accepting on that port (TCP simultaneous open).
//     Works when both NATs keep a stable mapping per local port.
//   - [StrategyPredictive]: for a NAT that allocates public ports
//     sequentially. The Listener (the stable side) scans a window of
//     PortScanRange ports centered on the Dialer's reflected port, while the
//     Dialer dials the Listener's stable endpoint.
//   - [StrategyBirthday]: for a NAT that allocates ports unpredictably. The
//     Listener dials many random ports on the Dialer's public IP from its
//     stable port, while the Dialer opens as many connections to the
//     Listener's endpoint from fresh local ports, relying on a birthday
//     collision between the two sets.
//   - [StrategyAuto]: the Dialer probes its NAT through the targets in
//     [Config.NATProbe] (at least three distinct signal servers), classifies
//     it as stable, sequential or dispersed, and the Listener picks direct,
//     predictive or birthday accordingly.
//
// Birthday and auto require the Listener to sit behind a stable-mapping NAT
// and to set [Config.StableSide]. A Dialer with an empty Strategy accepts
// whatever strategy the Listener picks; a Listener with an empty Strategy
// uses direct.
//
// # Connection delivery
//
// Direct, predictive and birthday with [CandidateSelectionFanout] deliver
// connections with no prefix: the first byte read is application data, and
// the upper-layer protocol is expected to detect and retry a dead candidate.
// Birthday with [CandidateSelectionACK] first exchanges a fixed-size,
// HMAC-authenticated selection frame so that one Dial and one Accept agree on
// the same connection; that frame is consumed before the connection is
// returned, so the caller still starts reading at the first application byte.
//
// # Packet API
//
// [ListenPacket] and [NewPacketDialer] are an experimental UDP counterpart,
// configured with [PacketConfig]. They yield a [PacketAttempt] whose Commit
// returns a [net.PacketConn] for an upper-layer protocol such as QUIC. This
// API may change.
package burrow
