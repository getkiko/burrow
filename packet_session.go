package burrow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

// This file chains signaling, STUN and candidate probing for the direct
// and predictive UDP strategies into one hole punching attempt that yields
// a *PacketAttempt.
//
// Socket phases: prepareUDPSocket completes the STUN mapping query first,
// and only after it has fully finished does udpSocketState.arm() switch the
// socket to the candidate phase. The later punching steps send candidate
// packets only on armed sockets and never touch STUN again.
//
// Proof ordering: the listener verifies the DIAL proof before acquireSockets
// allocates candidate sockets (establishPacketListener); the dialer verifies
// the READY/PEER proof before sending any candidate packet
// (establishPacketDialer).

// udpPreparedSocket is a local UDP socket that has finished its STUN
// mapping query but is not armed yet.
type udpPreparedSocket struct {
	conn       *net.UDPConn
	publicAddr netip.AddrPort
	state      *udpSocketState
}

func (p *udpPreparedSocket) Close() error {
	if p == nil || p.conn == nil {
		return nil
	}
	return p.conn.Close()
}

// prepareUDPSocket binds a new local UDP socket and runs one STUN mapping
// query on it. The returned socket is still in stunPhase; the caller must
// call state.arm() explicitly before sending candidate packets.
func prepareUDPSocket(ctx context.Context, cfg PacketConfig, bindAddr string) (*udpPreparedSocket, error) {
	conn, err := listenUDP(ctx, bindAddr, cfg)
	if err != nil {
		return nil, err
	}
	server := cfg.STUN.Servers[0]
	mapped, err := udpMappedAddr(ctx, conn, server, cfg.STUN.Timeout)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("burrow: UDP STUN mapping query: %w", err)
	}
	return &udpPreparedSocket{conn: conn, publicAddr: mapped, state: &udpSocketState{}}, nil
}

// proofContextFor assembles the fields a proof is computed over.
func proofContextFor(transport, room, session, endpoint string, strategy Strategy, fingerprint string) ProofContext {
	return ProofContext{
		Transport:   transport,
		Room:        room,
		Session:     session,
		Endpoint:    endpoint,
		Strategy:    strategy,
		Fingerprint: fingerprint,
	}
}

// signPacketProof computes the proof for an outgoing message when SignProof
// is set; otherwise it returns an empty string (no proof).
func signPacketProof(cfg PacketConfig, ctxValue ProofContext) (string, error) {
	if cfg.SignProof == nil {
		return "", nil
	}
	return cfg.SignProof(ctxValue)
}

// verifyPacketProof checks the proof of a peer message when VerifyProof is
// set. Without it every message passes, so hole punching works before the
// caller configures proofs.
func verifyPacketProof(cfg PacketConfig, ctxValue ProofContext, proof string) error {
	if cfg.VerifyProof == nil {
		return nil
	}
	return cfg.VerifyProof(ctxValue, proof)
}

// ErrProofRejected reports a failed session proof check. It is a uniform
// authentication error, returned before any candidate socket is allocated.
var ErrProofRejected = errors.New("burrow: session proof rejected")

// establishPacketDialer runs one UDP hole punching attempt on the dialer
// side.
func establishPacketDialer(ctx context.Context, cfg PacketConfig, signal *signalControl, session string, limiter *attemptLimiter, packetLimiter *candidatePacketLimiter) (*PacketAttempt, error) {
	bindAddr, err := udpSocketBindAddr(ctx, cfg)
	if err != nil {
		return nil, err
	}
	local, err := prepareUDPSocket(ctx, cfg, bindAddr)
	if err != nil {
		return nil, err
	}
	closeLocal := true
	defer func() {
		if closeLocal {
			_ = local.Close()
		}
	}()

	parameters := cfg.strategyParameters()
	fingerprint := parameters.FingerprintForTransport(signalproto.TransportUDP)
	endpoint := local.publicAddr.String()

	proof, err := signPacketProof(cfg, proofContextFor(signalproto.TransportUDP, cfg.Room, session, endpoint, cfg.Strategy, fingerprint))
	if err != nil {
		return nil, fmt.Errorf("burrow: sign DIAL proof: %w", err)
	}

	request := signalproto.Message{
		Room:        cfg.Room,
		Session:     session,
		Endpoint:    endpoint,
		Strategy:    cfg.Strategy,
		Parameters:  parameters,
		Fingerprint: fingerprint,
		Transport:   signalproto.TransportUDP,
		Proof:       proof,
	}
	pairCtx, cancel := context.WithTimeout(ctx, cfg.PairTimeout)
	peer, err := signal.dial(pairCtx, request)
	cancel()
	if err != nil {
		return nil, err
	}
	if err := validatePacketPeerStrategy(cfg, peer); err != nil {
		return nil, err
	}
	if err := checkPeerAddr(peer.Endpoint, endpoint); err != nil {
		return nil, err
	}
	// Verify the READY/PEER proof before sending candidate packets. A
	// failure must end the attempt before any candidate socket resources
	// are allocated: the only socket so far is local (used for STUN), which
	// the deferred cleanup closes, and acquireSockets below has not run.
	if err := verifyPacketProof(cfg, proofContextFor(signalproto.TransportUDP, cfg.Room, session, peer.Endpoint, peer.Strategy, peer.Fingerprint), peer.Proof); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProofRejected, err)
	}

	remotePublic, err := netip.ParseAddrPort(peer.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("burrow: invalid peer UDP address %q: %w", peer.Endpoint, err)
	}

	weight := strategySocketWeight(peer.Strategy, 0, cfg.Predictive.Concurrency)
	releaseSockets, err := limiter.acquireSockets(ctx, weight)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResourceLimit, err)
	}
	defer releaseSockets()

	attempt, err := armPacketAttempt(ctx, cfg, session, peer.Strategy, local, remotePublic, packetLimiter, roleDialer)
	if err != nil {
		return nil, err
	}
	closeLocal = false // ownership moved to attempt.Paths
	return attempt, nil
}

// validatePacketPeerStrategy checks that the strategy and parameters in the
// listener's reply match the local ones. It mirrors validatePeerStrategy on
// the TCP path but works on PacketConfig instead of Config.
func validatePacketPeerStrategy(cfg PacketConfig, peer signalproto.Message) error {
	if signalproto.ValidateEndpoint(peer.Endpoint) != nil {
		return ErrStrategyMismatch
	}
	switch peer.Strategy {
	case StrategyDirect, StrategyPredictive:
	default:
		return ErrStrategyMismatch
	}
	if peer.Strategy != cfg.Strategy {
		return ErrStrategyMismatch
	}
	if peer.Fingerprint != cfg.strategyParameters().FingerprintForTransport(signalproto.TransportUDP) {
		return ErrStrategyMismatch
	}
	return nil
}

// armPacketAttempt arms the STUN-mapped local socket for the candidate
// phase, punches according to the strategy and builds the PacketAttempt.
// self is the local role; for predictive it selects between scanning a
// window (the listener, which is the stable side) and aiming at the stable
// endpoint (the dialer), as on the TCP path.
func armPacketAttempt(ctx context.Context, cfg PacketConfig, session string, strategy Strategy, local *udpPreparedSocket, remotePublic netip.AddrPort, packetLimiter *candidatePacketLimiter, self role) (*PacketAttempt, error) {
	local.state.arm() // from here on the socket must not issue STUN queries

	punchTimeout := cfg.PunchTimeout
	if strategy == StrategyPredictive {
		punchTimeout = cfg.Predictive.AttemptTimeout
	}

	var remotes []netip.AddrPort
	var result *udpPunchResult
	var err error
	switch {
	case strategy == StrategyDirect:
		remotes = []netip.AddrPort{remotePublic}
		result, err = punchUDPSingle(ctx, local.state, packetLimiter, local.conn, remotePublic, defaultUDPProbeInterval, punchTimeout)
	case strategy == StrategyPredictive && self == roleListener:
		// Stable side: scan a port window centered on the peer's mapping.
		_, basePort, splitErr := splitUDPHostPort(remotePublic.String())
		if splitErr != nil {
			return nil, splitErr
		}
		ports := predictivePortSequence(basePort, cfg.Predictive.PortScanRange)
		remotes = make([]netip.AddrPort, 0, len(ports))
		for _, port := range ports {
			remotes = append(remotes, netip.AddrPortFrom(remotePublic.Addr(), uint16(port)))
		}
		result, err = scanUDPWindow(ctx, local.state, packetLimiter, local.conn, remotes, defaultUDPScanRoundInterval, punchTimeout)
	default:
		// Predictive changing side: aim at the stable endpoint reported by
		// the listener.
		remotes = []netip.AddrPort{remotePublic}
		result, err = punchUDPSingle(ctx, local.state, packetLimiter, local.conn, remotePublic, defaultUDPProbeInterval, punchTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("burrow: UDP hole punching failed (session=%s): %w", shortSession(session), errors.Join(err, ErrPunchFailed))
	}
	// result.remote is the address the first reply came from (diagnostic
	// only): remotePublic itself for direct and the aiming side, the port
	// that hit for a predictive scan. Candidate selection is still left to
	// the caller (Paths carries the whole candidate set, see packet.go), so
	// Remotes is not narrowed here.
	debugLogf(cfg.DebugLog, "burrow: UDP hole punching established a mapping (session=%s, strategy=%s, local=%s, hit=%s)",
		shortSession(session), strategy, local.publicAddr, result.remote)

	path := PacketPath{Conn: local.conn, PublicAddr: local.publicAddr, Remotes: remotes}
	return &PacketAttempt{
		Session:  session,
		Strategy: strategy,
		Paths:    []PacketPath{path},
	}, nil
}

const (
	defaultUDPProbeInterval     = 300 * time.Millisecond
	defaultUDPScanRoundInterval = 400 * time.Millisecond
)

// establishPacketListener runs one UDP hole punching attempt on the
// listener side.
//
// The proof must be verified before candidate sockets are allocated:
// request.Proof is checked first and limiter.acquireSockets runs only after
// it passes, so a failed check allocates nothing beyond the signaling
// connection itself.
func establishPacketListener(ctx context.Context, cfg PacketConfig, ctrl *listenerCtrl, request signalproto.Message, limiter *attemptLimiter, packetLimiter *candidatePacketLimiter) (*PacketAttempt, error) {
	strategy, err := selectPacketIncomingStrategy(cfg, request)
	if err != nil {
		return nil, err
	}

	// Verify the DIAL proof before allocating candidate sockets.
	if err := verifyPacketProof(cfg, proofContextFor(signalproto.TransportUDP, cfg.Room, request.Session, request.Endpoint, request.Strategy, request.Fingerprint), request.Proof); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProofRejected, err)
	}

	weight := strategySocketWeight(strategy, 0, cfg.Predictive.Concurrency)
	releaseSockets, err := limiter.acquireSockets(ctx, weight)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResourceLimit, err)
	}
	defer releaseSockets()

	bindAddr, err := udpSocketBindAddr(ctx, cfg)
	if err != nil {
		return nil, err
	}
	local, err := prepareUDPSocket(ctx, cfg, bindAddr)
	if err != nil {
		return nil, err
	}
	closeLocal := true
	defer func() {
		if closeLocal {
			_ = local.Close()
		}
	}()

	remotePublic, err := netip.ParseAddrPort(request.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("burrow: invalid peer UDP address %q: %w", request.Endpoint, err)
	}
	if err := checkPeerAddr(request.Endpoint, local.publicAddr.String()); err != nil {
		return nil, err
	}

	parameters := cfg.strategyParameters()
	fingerprint := parameters.FingerprintForTransport(signalproto.TransportUDP)
	endpoint := local.publicAddr.String()
	proof, err := signPacketProof(cfg, proofContextFor(signalproto.TransportUDP, cfg.Room, request.Session, endpoint, strategy, fingerprint))
	if err != nil {
		return nil, fmt.Errorf("burrow: sign READY proof: %w", err)
	}
	ready := signalproto.Message{
		Session:     request.Session,
		Endpoint:    endpoint,
		Strategy:    strategy,
		Parameters:  parameters,
		Fingerprint: fingerprint,
		Transport:   signalproto.TransportUDP,
		Proof:       proof,
	}
	// As on the TCP path, send READY before punching: the dialer starts
	// probing this endpoint as soon as it gets READY/PEER, while replying
	// only after punching would leave the dialer idle during this side's
	// probing. Arming is fast, so few early dialer probes are lost.
	ctrl.rememberReady(request.Session, ready)
	if err := ctrl.ready(ctx, ready); err != nil {
		return nil, err
	}

	attempt, err := armPacketAttempt(ctx, cfg, request.Session, strategy, local, remotePublic, packetLimiter, roleListener)
	if err != nil {
		return nil, err
	}
	closeLocal = false
	return attempt, nil
}

// selectPacketIncomingStrategy validates the dialer's DIAL message and picks
// the strategy the listener uses. Only direct and predictive are accepted,
// and both sides must use exactly the same Strategy and Fingerprint (auto
// negotiation is not supported over UDP).
func selectPacketIncomingStrategy(cfg PacketConfig, request signalproto.Message) (Strategy, error) {
	if signalproto.ValidateStrategy(request.Strategy) != nil {
		return "", ErrStrategyMismatch
	}
	switch request.Strategy {
	case StrategyDirect, StrategyPredictive:
	default:
		return "", fmt.Errorf("burrow: strategy %q is not supported by the packet API: %w", request.Strategy, ErrStrategyMismatch)
	}
	if request.Strategy != cfg.Strategy {
		return "", ErrStrategyMismatch
	}
	parameters := cfg.strategyParameters()
	if request.Fingerprint != parameters.FingerprintForTransport(signalproto.TransportUDP) {
		return "", ErrStrategyMismatch
	}
	return cfg.Strategy, nil
}
