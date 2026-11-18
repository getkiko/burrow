package burrow

import (
	"context"
	"errors"
	"sync"
)

// PacketListener accepts UDP hole punching attempts from peers.
//
// Unlike the TCP Listener it has no net.Listener semantics (UDP has no
// connection setup). AcceptPacket returns a whole *PacketAttempt: the set
// of candidate sockets that finished STUN, pairing and punching. Choosing
// a candidate is left to the caller.
type PacketListener struct {
	cfg           PacketConfig
	ctrl          *listenerCtrl
	limiter       *attemptLimiter
	packetLimiter *candidatePacketLimiter

	attempts chan packetAttemptResult
	cancel   context.CancelFunc
	done     chan struct{}

	wg        sync.WaitGroup
	closeOnce sync.Once
}

type packetAttemptResult struct {
	attempt *PacketAttempt
	session string
}

// ListenPacket starts a UDP hole punching listener. It returns immediately
// and accepts peer attempts in the background. The caller must call Close
// when done.
func ListenPacket(cfg PacketConfig) (*PacketListener, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctrl, err := newListenerCtrlWith(ctx, cfg.SignalAddr, cfg.Room, cfg.Mode, cfg.Control, cfg.ResolveIP,
		defaultAcceptQueue, cfg.DebugLog, cfg.InfoLog, "udp", packetListenCapacity(cfg), "tcp4")
	if err != nil {
		cancel()
		return nil, err
	}

	l := &PacketListener{
		cfg:           cfg,
		ctrl:          ctrl,
		limiter:       newAttemptLimiter(cfg.MaxConcurrentAttempts),
		packetLimiter: newCandidatePacketLimiter(),
		attempts:      make(chan packetAttemptResult, defaultAcceptQueue),
		cancel:        cancel,
		done:          make(chan struct{}),
	}
	go l.dispatch(ctx)
	return l, nil
}

// packetListenCapacity maps MaxConcurrentAttempts to the capacity carried
// by the LISTEN signaling message (the signal server accepts 1-16). On UDP
// the real limit is the socket budget rather than the number of room
// sessions as on TCP, so MaxConcurrentAttempts serves as an approximation.
func packetListenCapacity(cfg PacketConfig) int {
	if cfg.MaxConcurrentAttempts <= 0 {
		return 0
	}
	return min(cfg.MaxConcurrentAttempts, 16)
}

func (l *PacketListener) dispatch(ctx context.Context) {
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

func (l *PacketListener) handle(ctx context.Context, req incoming) {
	releaseSession, err := l.limiter.acquireSession(ctx)
	if err != nil {
		l.ctrl.forget(req.message.Session)
		return
	}
	defer releaseSession()

	attempt, err := establishPacketListener(ctx, l.cfg, l.ctrl, req.message, l.limiter, l.packetLimiter)
	if err != nil {
		_ = l.ctrl.reject(ctx, req.message.Session, packetListenerErrorCode(err), err.Error())
		l.ctrl.forget(req.message.Session)
		debugLogf(l.cfg.DebugLog, "packet-listener[%s] hole punching failed: %v", shortSession(req.message.Session), err)
		return
	}
	infoLogf(l.cfg.InfoLog, "burrow: UDP hole punching succeeded (room=%s, session=%s, strategy=%s, paths=%d)",
		l.cfg.Room, shortSession(req.message.Session), attempt.Strategy, len(attempt.Paths))
	select {
	case l.attempts <- packetAttemptResult{attempt: attempt, session: req.message.Session}:
	case <-ctx.Done():
		_ = attempt.Close()
	}
}

func packetListenerErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrProofRejected):
		return "invalid_proof"
	case errors.Is(err, ErrStrategyMismatch):
		return "strategy_mismatch"
	case errors.Is(err, ErrResourceLimit):
		return "room_busy"
	default:
		return "punch_failed"
	}
}

// AcceptPacket returns the next successful UDP candidate set. It returns
// ErrClosed after the PacketListener is closed.
func (l *PacketListener) AcceptPacket(ctx context.Context) (*PacketAttempt, error) {
	select {
	case result := <-l.attempts:
		return result.attempt, nil
	case <-l.done:
		select {
		case result := <-l.attempts:
			return result.attempt, nil
		default:
		}
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close stops listening and releases resources. It is safe to call more
// than once. PacketAttempts already returned by AcceptPacket are not
// affected; the caller must close them.
func (l *PacketListener) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		<-l.done
		l.wg.Wait()
		_ = l.ctrl.Close()
		for {
			select {
			case result := <-l.attempts:
				_ = result.attempt.Close()
			default:
				return
			}
		}
	})
	return nil
}

// ResetSignal drops the reused signaling connection; the background loop
// reconnects and re-registers on the current network.
func (l *PacketListener) ResetSignal() {
	l.ctrl.reset()
}
