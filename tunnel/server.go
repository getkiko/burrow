package tunnel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/getkiko/burrow"
)

// zombieCandidateTimeout is the fallback timeout for reaping "zombie"
// candidates.
//
// With the birthday fanout strategy the client side (runBirthdayDialer) uses
// only the first candidate that punches through and closes the rest in
// drainBirthdayHits, but the server side (burrow.Listener) may accept up to
// BirthdayOptions.MaxSuccessCandidates candidate connections. Without cleanup
// they would hold a goroutine and a socket forever.
//
// This is only a fallback. The primary rule is "dead on EOF/RST": a candidate
// closed by the client makes the server's first-byte Read return EOF or RST
// immediately, which is a deterministic signal and faster than any timeout.
// The timeout only covers peers that cannot send FIN/RST at all (process
// killed, cable pulled), so it can be long.
//
// It must not be a short "no bytes within N seconds" rule. That rule relied
// on the client punching only on the upstream's first Write, so a fresh hole
// always had data to send. With pre-punched pooled holes the assumption no
// longer holds: a pooled hole is by definition punched but unused, and idling
// for tens of seconds is normal. A short timeout would kill the whole pool.
//
// Ten minutes: pooled holes have no upper bound on idle time (it depends on
// when the user opens a connection), so the value only needs to be far above
// any reasonable idle period while still reclaiming fds after a hard kill.
const zombieCandidateTimeout = 10 * time.Minute

// punchListener abstracts the punching listener so tests can inject one.
// The production implementation is *burrow.Listener.
type punchListener interface {
	Accept() (net.Conn, error)
	Close() error
}

// Server forwards every punched hole, unchanged, to a fixed local TCP address.
//
// It does not parse what flows through the hole: the forward target is fixed
// by configuration and the payload is handled by the upper-layer protocol at
// the target. Each hole carries exactly one upstream connection (the client
// punches one hole per connection) as a raw byte stream.
type Server struct {
	forward  string
	listener punchListener
	tracker  *statusTracker
	logger   *Logger

	// zombieTimeout is the zombie candidate timeout; zero falls back to
	// zombieCandidateTimeout. It is a field so tests can inject a much
	// shorter value.
	zombieTimeout time.Duration

	// activeConns holds every candidate connection returned by
	// listener.Accept() that is still being handled by serveConn, so Close()
	// can close them.
	//
	// Registration must happen before serveConn is scheduled; otherwise, once
	// acceptLoop hands the conn to s.wg.Go(...), it is invisible to Close().
	// If Close() ran its Range in that window it would miss the conn:
	// listener.Close() does not affect accepted connections, so serveConn
	// would block on the first-byte read or on forwarding, wg.Wait() would
	// wait forever, and Close() would hang.
	//
	// Registering before scheduling is not enough by itself: acceptLoop and
	// Close() run concurrently, so "Accept() returns conn" and "Close() runs
	// Range" can still interleave with the Store landing after the Range, and
	// nobody would ever close that conn. registerMu therefore makes "check
	// closed" and "Store" one atomic step. Close() sets closed and runs Range
	// under the lock, so it sees every conn stored before that point;
	// acceptLoop checks closed under the same lock before Store and, if it is
	// set, closes the conn itself instead of storing it. Every conn is thus
	// either closed by Close()'s Range or by acceptLoop at registration time,
	// with no window where both miss it.
	registerMu  sync.Mutex
	closed      bool
	activeConns sync.Map // net.Conn -> struct{}

	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

// registerConn is called by acceptLoop after it gets a candidate connection
// and before serveConn is scheduled, and tries to add the conn to
// activeConns. It returns false if Close() has already run (or is running)
// its Range; the caller must then close the conn itself and not schedule
// serveConn, because Close() never runs a second Range.
func (s *Server) registerConn(conn net.Conn) bool {
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	if s.closed {
		return false
	}
	s.activeConns.Store(conn, struct{}{})
	return true
}

// NewServer builds a Server from cfg. observer may be nil.
func NewServer(cfg ServerConfig, observer Observer) (*Server, error) {
	if err := verifyInterface(cfg.BindInterface); err != nil {
		return nil, err
	}
	logger := newLoggerWithSink(cfg.LogLevel, cfg.LogSink)
	tracker := newStatusTracker(cfg.Room, observer)

	// NAT self-check: only the birthday strategy needs a stable port mapping
	// on the listener side, which a symmetric NAT cannot provide. Failing
	// early beats endless punch timeouts at run time. direct/predictive are
	// not affected; maybeCheckServerNAT decides from the strategy.
	if err := maybeCheckServerNAT(context.Background(), cfg.SkipNATCheck, cfg.Strategy, cfg.BindInterface, natProbe, logger); err != nil {
		return nil, err
	}

	burrowCfg := cfg.ToBurrowConfig()
	burrowCfg.DebugLog = logger.SDKDebugFunc()
	burrowCfg.InfoLog = logger.SDKInfoFunc()
	burrowCfg.StatsLog = tracker.setStats
	if len(cfg.Hosts) > 0 {
		burrowCfg.ResolveIP = staticHostsResolver(cfg.Hosts)
	}

	listener, err := burrow.Listen(burrowCfg)
	if err != nil {
		return nil, fmt.Errorf("create punch listener: %w", err)
	}

	return &Server{
		forward:       cfg.Forward,
		listener:      listener,
		tracker:       tracker,
		logger:        logger,
		zombieTimeout: zombieCandidateTimeout,
		done:          make(chan struct{}),
	}, nil
}

// Start begins accepting punched connections. It does not block.
func (s *Server) Start() error {
	s.logger.Infof("waiting for punched connections, forwarding to %s", s.forward)
	s.wg.Go(s.acceptLoop)
	return nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
			default:
				if !isNormalClose(err) {
					s.logger.Errorf("punch accept failed: %v", err)
				}
			}
			return
		}
		// With birthday fanout this returns several times in a row, one
		// independent candidate each, and usually only one of them is live.
		// sessionActivated must not be called here; it waits until serveConn
		// receives the first byte and proves the candidate usable.
		s.logger.Infof("punch candidate arrived local=%s remote=%s", conn.LocalAddr(), conn.RemoteAddr())
		// Register before wg.Go schedules serveConn. registerConn and Close()
		// share registerMu so registration and Close()'s Range cannot
		// interleave and both miss the conn (see the registerMu field). If
		// Close() already ran, registerConn returns false and the candidate
		// is closed here rather than starting a goroutine that would be
		// closed immediately.
		if !s.registerConn(conn) {
			conn.Close()
			continue
		}
		s.wg.Go(func() { s.serveConn(conn) })
	}
}

// serveConn handles one punched hole: once it is confirmed to be a live
// candidate, it is forwarded unchanged to the target address.
//
// With birthday fanout this connection is only one of several candidates and
// may not be the one the client uses. Until the first byte arrives it can
// neither be counted as live nor be waited on forever (that would leak a
// goroutine and a socket), so zombieTimeout acts as a fallback: a candidate
// that sends nothing before it expires is treated as a zombie and closed. See
// zombieCandidateTimeout for why the first byte is the signal.
func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	// acceptLoop registered this conn in activeConns via registerConn before
	// scheduling this goroutine; remove it on exit.
	defer s.activeConns.Delete(conn)

	timeout := s.zombieTimeout
	if timeout <= 0 {
		timeout = zombieCandidateTimeout
	}

	// The primary signal is EOF/RST; the timeout is only a fallback for peers
	// that cannot send FIN/RST. A read deadline is used instead of a timer
	// goroutine calling Close: on raw TCP "did a byte arrive" is simply the
	// result of one Read, so no other goroutine has to race for closing the
	// same conn.
	//
	// This must block until a real signal arrives and never give up early
	// because of a short wait: pre-punched client holes can idle in the pool
	// for any length of time, and closing them early kills live holes.
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		s.logger.Debugf("set first-byte deadline failed: %v", err)
	}
	first := make([]byte, 1)
	n, err := conn.Read(first)
	if n == 0 {
		// Ended without receiving a single byte. The two causes mean very
		// different things:
		//   - EOF/RST: the client closed this candidate (the normal fate of
		//     most fanout candidates); deterministic, debug level is enough.
		//   - timeout: nothing happened for the whole zombie timeout, so the
		//     peer was probably killed or lost its network. That is abnormal
		//     and worth a warning, so a fallback reap can be told apart from a
		//     normal close when troubleshooting.
		if isTimeoutError(err) {
			s.logger.Warnf("candidate sent no first byte within %s, reaping: %v", timeout, err)
		} else {
			s.logger.Debugf("candidate closed by peer, cleaning up: %v", err)
		}
		return
	}
	// A first byte means the candidate is live: clear the deadline and keep
	// it until the connection actually ends.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		s.logger.Debugf("clear first-byte deadline failed: %v", err)
	}

	s.tracker.sessionActivated(conn.LocalAddr().String(), conn.RemoteAddr().String())
	defer s.tracker.sessionDeactivated()
	s.logger.Infof("hole punched local=%s remote=%s", conn.LocalAddr(), conn.RemoteAddr())

	// The first byte has already been read from the hole and must be passed
	// on, or the upper-layer protocol loses a byte (a header-sensitive
	// protocol would fail its handshake outright).
	s.forwardConn(conn, first[:n], err)
}

// forwardConn forwards one hole unchanged to the configured target. pending
// holds the first byte(s) already read from the hole but not yet forwarded;
// readErr is the error returned by that read (it may be non-nil alongside
// data, and per the io.Reader contract the data is used first).
//
// streamOpened/streamClosed are paired before net.Dial so that the hole shows
// up in the total/active counters even when the target is unreachable.
// Otherwise a persistently unreachable target would show no traffic at all
// and look like "the tunnel is not forwarding" instead of "the forward target
// is down".
func (s *Server) forwardConn(conn net.Conn, pending []byte, readErr error) {
	s.tracker.streamOpened()
	defer s.tracker.streamClosed()

	target, err := net.Dial("tcp", s.forward)
	if err != nil {
		// The forward target is unreachable (for example, the service is not
		// running). Close the hole without retrying; the upper-layer protocol
		// on the client side reconnects on its own.
		s.logger.Warnf("dial forward target %s failed: %v", s.forward, err)
		return
	}
	defer target.Close()

	// Forward open/close is logged at info, not debug: this is the only place
	// on the server where one can see that data actually reached the exit.
	// With the default log_level=info, a debug-only log would leave users
	// unable to tell whether traffic got this far when a hole is up but
	// nothing works.
	s.logger.Infof("forward connection established -> %s", s.forward)

	var fromTunnel int64
	if len(pending) > 0 {
		wn, werr := target.Write(pending)
		fromTunnel += int64(wn)
		if wn > 0 {
			s.tracker.addAppTraffic(uint64(wn), 0)
		}
		if werr != nil {
			s.logger.Warnf("forward first byte failed: %v", werr)
			return
		}
	}
	// The first-byte read already failed (for example, the peer sent one byte
	// and disconnected). The byte has been forwarded; there is no point in
	// entering relay.
	if readErr != nil {
		s.logger.Infof("forward connection closed -> %s in=%dB out=%dB", s.forward, fromTunnel, 0)
		return
	}

	// relayCounted rather than relay plus a final sum: relay only returns when
	// the connection ends, so a long-lived connection would show zero traffic
	// and speed until it closed and then jump. Here every written chunk is
	// reported as it happens.
	//
	// Direction: conn->target comes in from the tunnel (rx), target->conn is
	// the target's reply going back into the tunnel (tx).
	rest, fromTarget := relayCounted(
		conn, target,
		func(n uint64) { s.tracker.addAppTraffic(n, 0) },
		func(n uint64) { s.tracker.addAppTraffic(0, n) },
	)
	fromTunnel += rest
	s.logger.Infof("forward connection closed -> %s in=%dB out=%dB", s.forward, fromTunnel, fromTarget)
}

// Snapshot returns a snapshot of the current status.
func (s *Server) Snapshot() Status {
	return s.tracker.Snapshot()
}

// Close shuts down the Server. It is safe to call more than once.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.listener.Close()

		// Setting closed and running Range under registerMu is the key step:
		// registerConn takes the same lock and checks closed before Store, so
		// after this no entry can be stored without being seen by this Range
		// (see the registerMu field for the full TOCTOU discussion).
		s.registerMu.Lock()
		s.closed = true
		// Close every candidate still in serveConn. Closing the listener only
		// stops new candidates; accepted ones blocked on the first-byte read
		// or on forwarding are unaffected, so each conn must be closed or
		// wg.Wait() would wait forever (or up to zombieTimeout). A serveConn
		// blocked in conn.Read gets an error and returns at once.
		// conn.Close() is idempotent, so it does not conflict with serveConn's
		// own deferred Close.
		s.activeConns.Range(func(key, _ any) bool {
			key.(net.Conn).Close()
			return true
		})
		s.registerMu.Unlock()
	})
	s.wg.Wait()
	return nil
}
