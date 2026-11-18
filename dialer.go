package burrow

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

// Dialer opens connections to a Listener through TCP hole punching.
//
// Every Dial runs a full round of signaling and punching and returns an
// independent net.Conn, so it can be called repeatedly to open several
// connections. A Dialer is safe for concurrent use.
type Dialer struct {
	cfg     Config
	signal  *signalControl
	limiter *attemptLimiter

	// ctx spans the Dialer's lifetime and is canceled by Close to abort
	// in-flight Dials. Otherwise punching goroutines would keep holding local
	// ports after Close returns, leaking over time in long-lived processes
	// that reload their configuration.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool

	profileMu       sync.Mutex
	profile         *cachedNATProfile
	profileFailures int
}

// NewDialer returns a Dialer for cfg. Call Close when done.
func NewDialer(cfg Config) (*Dialer, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	if cfg.Strategy == StrategyAuto && distinctProbeTargets(cfg.NATProbe.Targets) < 3 {
		return nil, errors.New("burrow: auto strategy requires at least three NAT probe targets")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dialer{
		cfg:     cfg,
		signal:  newSignalControl(cfg.SignalAddr, cfg.Room, cfg.Mode, cfg.Control, cfg.ResolveIP, cfg.AcceptQueue, cfg.tcpNetwork()),
		limiter: newAttemptLimiter(cfg.MaxConcurrentAttempts),
		ctx:     ctx,
		cancel:  cancel,
	}, nil
}

// Dial opens a connection to the peer listening in the same room.
//
// Each call uses its own session ID and runs the full flow, so concurrent
// calls open independent connections with no shared state.
//
// ctx bounds the whole round (pairing and punching); Dial returns as soon as
// ctx is canceled. Use errors.Is on the error: ErrNoListener means nobody is
// listening (retrying is pointless), ErrPunchFailed means punching failed
// (retrying may help).
func (d *Dialer) Dial(ctx context.Context) (net.Conn, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, ErrClosed
	}
	// Register under the lock, mutually exclusive with Close setting closed,
	// so no new Dial can slip in after Close (wg.Wait would miss it).
	d.wg.Add(1)
	d.mu.Unlock()
	defer d.wg.Done()

	session, err := newSessionID()
	if err != nil {
		return nil, err
	}
	startedAt := time.Now()

	// Tie the caller's ctx to the Dialer lifetime: either one canceling ends
	// this round.
	ctx, cancel := mergeCancel(ctx, d.ctx)
	defer cancel()
	releaseSession, err := d.limiter.acquireSession(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseSession()

	var profile *signalproto.NATProfile
	if d.cfg.Strategy == StrategyAuto {
		profileValue, profileErr := d.natProfile(ctx)
		if profileErr != nil {
			return nil, profileErr
		}
		profile = &profileValue
	}
	conn, err := establishDialer(ctx, d.cfg, d.signal, session, profile, d.limiter)
	if errors.Is(err, errNATProfileStale) {
		d.resetNATProfile()
		profileValue, profileErr := d.natProfile(ctx)
		if profileErr != nil {
			return nil, profileErr
		}
		conn, err = establishDialer(ctx, d.cfg, d.signal, session, &profileValue, d.limiter)
	}
	if d.cfg.Strategy == StrategyAuto {
		d.recordProfileOutcome(err)
	}
	if err != nil && d.ctx.Err() != nil {
		// Failures caused by Close are reported as ErrClosed rather than
		// exposing internal timeouts.
		return nil, ErrClosed
	}
	if err == nil {
		infoLogf(d.cfg.InfoLog, "burrow: punch succeeded (room=%s, session=%s, local=%s, remote=%s, took %s)",
			d.cfg.Room, shortSession(session), conn.LocalAddr(), conn.RemoteAddr(),
			time.Since(startedAt).Round(time.Millisecond))
		startTCPStatsMonitor(d.cfg.DebugLog, d.cfg.StatsLog, roleDialer, session, conn)
	}
	return conn, err
}

// Close closes the Dialer and aborts all in-flight Dials. Connections already
// returned are unaffected and must be closed by the caller. Close is
// idempotent.
func (d *Dialer) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()

	d.cancel() // abort in-flight punching and release its local ports
	err := d.signal.close()
	d.wg.Wait() // wait for in-flight Dials so no goroutines remain
	return err
}

// mergeCancel returns a context canceled when either a or b is canceled.
func mergeCancel(a, b context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}

// ResetSignal drops the shared signaling connection and the cached NAT
// profile; the next operation reconnects on the current network.
func (d *Dialer) ResetSignal() {
	d.signal.reset()
	d.resetNATProfile()
}
