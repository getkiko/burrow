package burrow

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// Listener accepts connections from peers through TCP hole punching. It
// implements net.Listener.
//
// It is event driven: a long-running goroutine receives connection requests
// from the shared signaling channel, and each request runs a punching attempt
// bounded by the session limit and the candidate socket budget.
//
// Each request carries its own session and shares no state with others, so
// pairing never races.
type Listener struct {
	cfg     Config
	ctrl    *listenerCtrl
	limiter *attemptLimiter

	conns  chan net.Conn
	cancel context.CancelFunc
	done   chan struct{} // closed when the dispatch loop exits

	wg        sync.WaitGroup // tracks in-flight punching goroutines
	closeOnce sync.Once
}

// Listener implements net.Listener.
var _ net.Listener = (*Listener)(nil)

// Listen starts a Listener for cfg. It returns immediately and accepts peers
// in the background. Call Close when done.
func Listen(cfg Config) (*Listener, error) {
	cfg, err := cfg.normalizeListener()
	if err != nil {
		return nil, err
	}
	if (cfg.Strategy == StrategyAuto || cfg.Strategy == StrategyBirthday) && !cfg.StableSide {
		return nil, errors.New("burrow: auto/birthday Listener requires stable_side")
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctrl, err := newListenerCtrl(ctx, cfg)
	if err != nil {
		cancel()
		return nil, err
	}

	l := &Listener{
		cfg:     cfg,
		ctrl:    ctrl,
		limiter: newAttemptLimiter(cfg.MaxConcurrentAttempts),
		conns:   make(chan net.Conn, cfg.AcceptQueue),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go l.dispatch(ctx)
	return l, nil
}

func listenerErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrUnsupportedNATPair):
		return "unsupported_nat_pair"
	case errors.Is(err, ErrStrategyMismatch):
		return "strategy_mismatch"
	case errors.Is(err, ErrResourceLimit):
		return "room_busy"
	default:
		return "punch_failed"
	}
}

// dispatch receives connection requests and starts a punching goroutine for
// each one.
func (l *Listener) dispatch(ctx context.Context) {
	defer close(l.done)
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-l.ctrl.incomingCh:
			duplicate, ready := l.ctrl.markSeen(req.message.Session)
			if duplicate {
				if ready != nil {
					l.wg.Go(func() { _ = l.ctrl.ready(ctx, *ready) })
				}
				continue
			}
			l.wg.Go(func() { l.handle(ctx, req) })
		}
	}
}

// handle runs one punching attempt and hands the connections to Accept.
//
// A failure affects neither other requests nor the Listener; the peer
// retries.
func (l *Listener) handle(ctx context.Context, req incoming) {
	releaseSession, err := l.limiter.acquireSession(ctx)
	if err != nil {
		l.ctrl.forget(req.message.Session)
		return
	}
	defer releaseSession()
	startedAt := time.Now()
	err = establishListener(ctx, l.cfg, l.ctrl, req.message, l.limiter, func(conn net.Conn) error {
		select {
		case l.conns <- conn:
			infoLogf(l.cfg.InfoLog, "burrow: punch succeeded (room=%s, session=%s, local=%s, remote=%s, took %s)",
				l.cfg.Room, shortSession(req.message.Session), conn.LocalAddr(), conn.RemoteAddr(),
				time.Since(startedAt).Round(time.Millisecond))
			startTCPStatsMonitor(l.cfg.DebugLog, l.cfg.StatsLog, roleListener, req.message.Session, conn)
			return nil
		case <-ctx.Done():
			_ = conn.Close()
			return ErrClosed
		}
	})
	if err != nil {
		_ = l.ctrl.reject(ctx, req.message.Session, listenerErrorCode(err), err.Error())
		// Clear the dedup mark only on failure: the peer keeps resending
		// DIAL, and a later INCOMING must be able to trigger a new attempt,
		// otherwise the session is never served.
		//
		// On success the mark must stay until sessionRetention expires. dial()
		// resends DIAL every 2s regardless, and the server redelivers INCOMING
		// for an existing session. Without the mark the same Dial would be
		// punched again, piling up unclaimed connections on the Accept side
		// until timeout or fd exhaustion.
		l.ctrl.forget(req.message.Session)
		debugLogf(l.cfg.DebugLog, "listener[%s] punch failed: %v", shortSession(req.message.Session), err)
	}
}

// Accept returns the next punched connection. After Close it returns
// ErrClosed.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		// The dispatch loop has exited (Close was called), but the buffer may
		// still hold ready connections.
		select {
		case conn := <-l.conns:
			return conn, nil
		default:
		}
		return nil, ErrClosed
	}
}

// Close stops listening and releases resources. It is idempotent.
// Connections already returned by Accept are unaffected and must be closed by
// the caller.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		<-l.done    // wait for dispatch to exit so no new attempts start
		l.wg.Wait() // wait for in-flight attempts to finish
		_ = l.ctrl.Close()

		// Drain connections nobody accepted to avoid leaking fds. Do not
		// close(l.conns): a concurrent Accept would read a nil connection
		// from the closed channel. Drain without blocking instead; there are
		// no writers left at this point.
		for {
			select {
			case c := <-l.conns:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return nil
}

// ResetSignal drops the shared signaling connection; the background loop
// reconnects on the current network and registers again.
func (l *Listener) ResetSignal() {
	l.ctrl.reset()
}

// Addr returns the signaling-level address of the Listener. Every punched
// connection uses a different local port, so this identifies the room rather
// than a socket.
func (l *Listener) Addr() net.Addr {
	return roomAddr{signal: l.cfg.SignalAddr, room: l.cfg.Room}
}

// roomAddr describes a room as a net.Addr.
type roomAddr struct {
	signal string
	room   string
}

func (a roomAddr) Network() string { return "burrow-tcp" }
func (a roomAddr) String() string  { return a.signal + "/" + a.room }
