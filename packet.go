package burrow

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// This file defines the result types of the UDP packet API: PacketPath,
// PacketAttempt, Commit and Close.
//
// A PacketAttempt hands over a set of sockets that have finished STUN,
// pairing and candidate preparation. It does not pick a candidate: deciding
// which path actually got through is left to the upper-layer protocol (for
// example QUIC), which sends real handshake packets on the candidates and
// calls Commit once one of them authenticates.

// PacketPath is a candidate path: a bound local UDP socket whose public
// mapping is known from STUN.
type PacketPath struct {
	// Conn is the bound local UDP socket.
	Conn *net.UDPConn

	// PublicAddr is the public mapping of Conn as reported by STUN.
	PublicAddr netip.AddrPort

	// Remotes are the peer candidate addresses to try on this path:
	//   - direct: exactly one address (the peer's only STUN mapping).
	//   - predictive, scanning side: one exact address (the peer's stable
	//     mapped endpoint).
	//   - predictive, stable side: the whole port window produced by
	//     predictivePortSequence (expanding outward from the center, see
	//     packet_predictive.go).
	Remotes []netip.AddrPort
}

// PacketAttempt is the set of candidate sockets produced by one hole
// punching attempt.
//
// When DialPacket or AcceptPacket returns, every socket in Paths is bound,
// has a STUN mapping, and the signaling-level proof check has passed. The
// caller probes the candidates with its own protocol's packets, calls
// Commit to keep the winning path and close the rest, and must eventually
// call Close (safe even after Commit) so that no socket leaks.
type PacketAttempt struct {
	Session  string
	Strategy Strategy
	// User is reserved for multi-user listeners that need to locate a
	// per-user key. It is always 0 for now (single-user listener).
	User  int
	Paths []PacketPath

	mu        sync.Mutex
	committed bool
	closed    bool
}

// Commit keeps the socket of Paths[path], closes all other sockets and
// returns the winner for further use. peer records the chosen remote
// address for logging and diagnostics only; Commit does not validate it,
// because the correctness of candidate selection is the upper-layer
// protocol's job (for example its authenticated handshake).
//
// The losing sockets must be released promptly (within one second). They
// are closed synchronously here: net.UDPConn.Close does not block, so no
// goroutine or timer is needed.
func (a *PacketAttempt) Commit(path int, peer netip.AddrPort) (net.PacketConn, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, ErrClosed
	}
	if path < 0 || path >= len(a.Paths) {
		return nil, fmt.Errorf("burrow: Commit path index %d out of range (%d paths)", path, len(a.Paths))
	}
	if a.committed {
		return nil, fmt.Errorf("burrow: PacketAttempt already committed")
	}
	winner := a.Paths[path].Conn
	for index := range a.Paths {
		if index == path || a.Paths[index].Conn == nil {
			continue
		}
		_ = a.Paths[index].Conn.Close()
		// Clear it so that a later Close does not close the same socket
		// again and report "use of closed network connection".
		a.Paths[index].Conn = nil
	}
	a.committed = true
	_ = peer // diagnostic only, see the doc comment
	return winner, nil
}

// Close releases every socket held by the attempt. It is safe to call more
// than once and after Commit. After Commit only the winning socket is still
// open, and Close closes it too, so a caller that keeps using the
// PacketConn returned by Commit must not call Close until it is done with
// that connection.
func (a *PacketAttempt) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	var firstErr error
	for _, path := range a.Paths {
		if path.Conn == nil {
			continue
		}
		if err := path.Conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
