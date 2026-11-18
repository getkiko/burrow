package burrow

import (
	"context"
	"sync"
)

// PacketDialer starts UDP hole punching attempts towards a peer.
//
// Each DialPacket runs a full round of signaling, pairing, STUN mapping and
// candidate punching, and returns a *PacketAttempt: the set of prepared
// candidate sockets. Choosing which socket to keep is left to the caller.
type PacketDialer struct {
	cfg           PacketConfig
	signal        *signalControl
	limiter       *attemptLimiter
	packetLimiter *candidatePacketLimiter

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

// NewPacketDialer creates a UDP hole punching dialer. Call Close when done.
func NewPacketDialer(cfg PacketConfig) (*PacketDialer, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &PacketDialer{
		cfg:           cfg,
		signal:        newSignalControl(cfg.SignalAddr, cfg.Room, cfg.Mode, cfg.Control, cfg.ResolveIP, defaultAcceptQueue, "tcp4"),
		limiter:       newAttemptLimiter(cfg.MaxConcurrentAttempts),
		packetLimiter: newCandidatePacketLimiter(),
		ctx:           ctx,
		cancel:        cancel,
	}, nil
}

// DialPacket runs one UDP hole punching attempt. ctx bounds the whole round
// (pairing and punching); DialPacket returns as soon as ctx is done or the
// PacketDialer is closed.
//
// Errors can be tested with errors.Is: ErrNoListener means the peer is not
// listening, ErrProofRejected means the proof check failed (and no
// candidate socket was allocated), and ErrPunchFailed means punching did
// not succeed.
func (d *PacketDialer) DialPacket(ctx context.Context) (*PacketAttempt, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, ErrClosed
	}
	d.wg.Add(1)
	d.mu.Unlock()
	defer d.wg.Done()

	session, err := newSessionID()
	if err != nil {
		return nil, err
	}

	ctx, cancel := mergeCancel(ctx, d.ctx)
	defer cancel()
	releaseSession, err := d.limiter.acquireSession(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseSession()

	attempt, err := establishPacketDialer(ctx, d.cfg, d.signal, session, d.limiter, d.packetLimiter)
	if err != nil && d.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if err == nil {
		infoLogf(d.cfg.InfoLog, "burrow: UDP hole punching succeeded (room=%s, session=%s, strategy=%s, paths=%d)",
			d.cfg.Room, shortSession(session), attempt.Strategy, len(attempt.Paths))
	}
	return attempt, err
}

// Close closes the PacketDialer and aborts all in-flight DialPacket calls.
// PacketAttempts already returned are not affected; the caller must close
// them. Close is safe to call more than once.
func (d *PacketDialer) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()

	d.cancel()
	err := d.signal.close()
	d.wg.Wait()
	return err
}

// ResetSignal drops the reused signaling connection; the next operation
// reconnects on the current network.
func (d *PacketDialer) ResetSignal() {
	d.signal.reset()
}
