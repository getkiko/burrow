package burrow

import (
	"context"
	"fmt"
	"net"

	"github.com/getkiko/burrow/internal/signalproto"
)

// prepared keeps the mapping lease alive so every socket in one attempt can
// reuse the endpoint advertised through Signal.
type prepared struct {
	localAddr string
	publicTCP string
	token     string
	lease     *mappingLease
}

func (p *prepared) Close() error {
	if p == nil || p.lease == nil {
		return nil
	}
	return p.lease.Close()
}

func prepareLocal(ctx context.Context, cfg Config, session string) (prepared, error) {
	interfaceName := cfg.interfaceName()
	tcpNetwork := cfg.tcpNetwork()
	ip, err := bindIP(ctx, interfaceName, cfg.SignalAddr, tcpNetwork, cfg.Control, cfg.ResolveIP, cfg.InterfaceAddr, cfg.DebugLog)
	if err != nil {
		return prepared{}, err
	}
	debugLogf(cfg.DebugLog, "prepareLocal: bindIP(interface=%q) => %s (control!=nil=%v)", interfaceName, ip, cfg.Control != nil)
	localAddr, err := allocLocalPort(ip, tcpNetwork, cfg.Control)
	if err != nil {
		return prepared{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, defaultSignalTimeout)
	defer cancel()
	client := &signalClient{addr: cfg.SignalAddr, mode: cfg.Mode, control: cfg.Control, resolveIP: cfg.ResolveIP, tcpNetwork: tcpNetwork}
	lease, err := client.openMappingLease(probeCtx, cfg.SignalAddr, localAddr, cfg.Room, session)
	if err != nil {
		return prepared{}, err
	}
	return prepared{
		localAddr: localAddr,
		publicTCP: lease.Endpoint,
		token:     lease.Token,
		lease:     lease,
	}, nil
}

func establishDialer(
	ctx context.Context,
	cfg Config,
	signal *signalControl,
	session string,
	profile *signalproto.NATProfile,
	limiter *attemptLimiter,
) (net.Conn, error) {
	local, err := prepareLocal(ctx, cfg, session)
	if err != nil {
		return nil, err
	}
	defer local.Close()
	if profile != nil && profile.PublicIP != "" {
		currentPublicIP, _, splitErr := net.SplitHostPort(local.publicTCP)
		if splitErr != nil {
			return nil, splitErr
		}
		if currentPublicIP != profile.PublicIP {
			return nil, fmt.Errorf("cached=%s current=%s: %w", profile.PublicIP, currentPublicIP, errNATProfileStale)
		}
	}

	// Direct and predictive need a listener before DIAL is published. Birthday
	// starts only after the stable side has armed its candidate set and sent READY.
	p, err := newPuncher(ctx, local.localAddr, local.publicTCP, roleDialer, session, cfg.punchOptions(StrategyDirect, roleDialer))
	if err != nil {
		return nil, err
	}
	defer p.Close()

	selectionKey, err := newSelectionKey()
	if err != nil {
		return nil, err
	}
	parameters := cfg.strategyParameters()
	wireStrategy := cfg.Strategy
	if cfg.legacyStrategy {
		wireStrategy = StrategyAuto
	}
	request := signalproto.Message{
		Room:         cfg.Room,
		Session:      session,
		Endpoint:     local.publicTCP,
		Token:        local.token,
		Profile:      profile,
		Strategy:     wireStrategy,
		Parameters:   parameters,
		Fingerprint:  parameters.Fingerprint(),
		SelectionKey: selectionKey,
	}
	pairCtx, cancel := context.WithTimeout(ctx, cfg.PairTimeout)
	peer, err := signal.dial(pairCtx, request)
	cancel()
	if err != nil {
		return nil, err
	}
	strategy, err := validatePeerStrategy(cfg, peer)
	if err != nil {
		return nil, err
	}
	if err := checkPeerAddr(peer.Endpoint, local.publicTCP); err != nil {
		return nil, err
	}
	// Pairing is one of the slowest phases (it waits for the peer to join the
	// room), so it is logged at info level; otherwise the user sees nothing
	// during the wait.
	infoLogf(cfg.InfoLog, "burrow: paired (session=%s, strategy=%s, peer=%s), punching",
		shortSession(session), strategy, peer.Endpoint)

	releaseSockets, err := limiter.acquireSockets(ctx, strategySocketWeight(strategy, cfg.Birthday.Candidates, cfg.Predictive.Concurrency))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResourceLimit, err)
	}
	defer releaseSockets()

	if strategy == StrategyBirthday {
		_ = p.Close()
		conn, birthdayErr := runBirthdayDialer(ctx, cfg, session, local.localAddr, peer.Endpoint, selectionKey)
		if birthdayErr != nil {
			return nil, birthdayErr
		}
		debugLogf(cfg.DebugLog, "dialer[%s] birthday using connection %s->%s", shortSession(session), conn.LocalAddr(), conn.RemoteAddr())
		return conn, nil
	}

	p.opt = cfg.punchOptions(strategy, roleDialer)
	res, err := p.punch(ctx, peer.Endpoint)
	if err != nil {
		return nil, err
	}
	debugLogf(cfg.DebugLog, "dialer[%s] using connection passive=%v %s->%s",
		shortSession(session), res.passive, res.conn.LocalAddr(), res.conn.RemoteAddr())
	return res.conn, nil
}

func establishListener(
	ctx context.Context,
	cfg Config,
	ctrl *listenerCtrl,
	request signalproto.Message,
	limiter *attemptLimiter,
	yield func(net.Conn) error,
) error {
	// Negotiated parameters always come from the peer (Listen rejects them on
	// the Listener), so derive the Config for this attempt first and read
	// everything from it.
	cfg, err := cfg.withParameters(request.Parameters)
	if err != nil {
		return err
	}
	strategy, err := selectIncomingStrategy(cfg, request)
	if err != nil {
		return err
	}
	if strategy == StrategyBirthday {
		if err := cfg.checkBirthdayDeadline(); err != nil {
			return err
		}
	}
	if strategy == StrategyBirthday && cfg.Birthday.Selection == CandidateSelectionACK {
		if _, err := decodeSelectionKey(request.SelectionKey); err != nil {
			return err
		}
	}
	debugLogf(cfg.DebugLog, "listener[%s] strategy=%s peer_profile=%+v effective parameters (from peer): candidates=%d ports=%d-%d warmup=%s timeout=%s grace=%s max_success=%d selection=%s predictive_range=%d",
		shortSession(request.Session), strategy, request.Profile,
		cfg.Birthday.Candidates, cfg.Birthday.MinPort, cfg.Birthday.MaxPort,
		cfg.Birthday.Warmup, cfg.Birthday.AttemptTimeout, cfg.Birthday.SuccessGrace,
		cfg.Birthday.MaxSuccessCandidates, cfg.Birthday.Selection, cfg.Predictive.PortScanRange)
	releaseSockets, err := limiter.acquireSockets(ctx, strategySocketWeight(strategy, cfg.Birthday.Candidates, cfg.Predictive.Concurrency))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrResourceLimit, err)
	}
	defer releaseSockets()

	local, err := prepareLocal(ctx, cfg, request.Session)
	if err != nil {
		return err
	}
	defer local.Close()
	if err := checkPeerAddr(request.Endpoint, local.publicTCP); err != nil {
		return err
	}

	ready := signalproto.Message{
		Session:      request.Session,
		Endpoint:     local.publicTCP,
		Strategy:     strategy,
		Parameters:   cfg.strategyParameters(),
		Fingerprint:  cfg.strategyParameters().Fingerprint(),
		SelectionKey: request.SelectionKey,
	}
	announce := func() error {
		ctrl.rememberReady(request.Session, ready)
		return ctrl.ready(ctx, ready)
	}

	if strategy == StrategyBirthday {
		return runBirthdayListener(ctx, cfg, request.Session, local.localAddr, request.Endpoint, request.SelectionKey, announce, yield)
	}

	p, err := newPuncher(ctx, local.localAddr, local.publicTCP, roleListener, request.Session, cfg.punchOptions(strategy, roleListener))
	if err != nil {
		return err
	}
	defer p.Close()
	if err := announce(); err != nil {
		return err
	}
	return p.punchCandidates(ctx, request.Endpoint, 2, listenerCandidateGrace, func(res *punchResult) error {
		debugLogf(cfg.DebugLog, "listener[%s] delivering candidate passive=%v %s->%s",
			shortSession(request.Session), res.passive, res.conn.LocalAddr(), res.conn.RemoteAddr())
		return yield(res.conn)
	})
}

func selectIncomingStrategy(cfg Config, request signalproto.Message) (Strategy, error) {
	if signalproto.ValidateStrategy(request.Strategy) != nil {
		return "", ErrStrategyMismatch
	}
	// The peer sends the parameters, so check that the request is
	// self-consistent (the fingerprint matches its parameters) rather than
	// equal to the local configuration.
	if request.Fingerprint == "" || request.Fingerprint != request.Parameters.Fingerprint() {
		return "", ErrStrategyMismatch
	}
	if cfg.Strategy != StrategyAuto {
		if request.Strategy != StrategyAuto && request.Strategy != cfg.Strategy {
			return "", ErrStrategyMismatch
		}
		if cfg.Strategy == StrategyBirthday && !cfg.StableSide {
			return "", fmt.Errorf("birthday requires stable_side on Listener: %w", ErrUnsupportedNATPair)
		}
		return cfg.Strategy, nil
	}
	if request.Strategy != StrategyAuto {
		if request.Strategy == StrategyBirthday && !cfg.StableSide {
			return "", fmt.Errorf("birthday requires stable_side on Listener: %w", ErrUnsupportedNATPair)
		}
		return request.Strategy, nil
	}
	if request.Profile == nil {
		return "", ErrUnsupportedNATPair
	}
	return selectStrategy(cfg.Strategy, cfg.StableSide, *request.Profile)
}

func validatePeerStrategy(cfg Config, peer signalproto.Message) (Strategy, error) {
	if signalproto.ValidateEndpoint(peer.Endpoint) != nil || signalproto.ValidateStrategy(peer.Strategy) != nil || peer.Strategy == StrategyAuto {
		return "", ErrStrategyMismatch
	}
	if peer.Fingerprint != cfg.strategyParameters().Fingerprint() {
		return "", ErrStrategyMismatch
	}
	if !cfg.legacyStrategy && cfg.Strategy != StrategyAuto && peer.Strategy != cfg.Strategy {
		return "", ErrStrategyMismatch
	}
	return peer.Strategy, nil
}

func checkPeerAddr(peer, self string) error {
	if peer == self {
		return fmt.Errorf("signal server returned our own address as the peer (%s)", peer)
	}
	return nil
}
