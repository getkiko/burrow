package tunnel

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getkiko/burrow"
)

// Punch retry parameters.
//
// The birthday strategy relies on a birthday collision: a set of candidate
// ports against the whole port space gives a per-round hit rate far below
// 100%, so success comes from many consecutive rounds. The reference
// experiment (cmd/burrow/birthday_client.go) runs 100 trials by default, each
// with fresh connections, moving to the next round immediately on failure.
//
// So a failed punch is never backed off: backoff defeats the collision
// mechanism, and a slightly unfavorable NAT pair (e.g. one restricted cone,
// one port-restricted cone) would never connect as retries slow down.
//
// Only two errors make an immediate retry pointless:
//   - ErrNoListener: the peer is not online; retrying just spins the signal server
//   - ErrResourceLimit: the signal server is already rate limiting us
//
// For those we wait a short fixed delay (as pairWithRetry in
// birthday_client.go does, which also waits 1s on ErrNoListener), with no
// exponential growth.
const (
	// defaultTrials is the default number of consecutive punch rounds per
	// connection attempt. The config field trials overrides it (matches the
	// -trials default of cmd/burrow/birthday_client.go).
	defaultTrials = 100

	// idleRetryDelay is the fixed wait when the peer is offline or the
	// signal server rate limits us. It does not grow.
	idleRetryDelay = time.Second
)

// errManagerClosed reports that the sessionManager has been closed.
var errManagerClosed = errors.New("tunnel: session manager closed")

// punchDialer abstracts the punch dialer so tests can inject one. The
// production implementation is *burrow.Dialer.
type punchDialer interface {
	Dial(ctx context.Context) (net.Conn, error)
}

// sessionManager punches one hole per upstream connection: one-to-one, with
// no multiplexing and no sharing of a hole between connections.
//
// Why not a single shared tunnel: an earlier version ran smux over the one
// punched TCP connection, but smux handshakes and keepalive frames change the
// traffic fingerprint. A punched TCP connection must carry a raw byte stream
// with no custom headers, handshakes or heartbeats. Without smux there are no
// streams, so each upstream connection owns a hole and the hole carries only
// the upper-layer protocol's own bytes.
//
// Punching is still deferred until data first needs to flow (triggered by the
// first Read/Write on a lazyConn): the birthday fanout strategy identifies the
// usable candidate by which one receives real data first, so this moment
// cannot be moved earlier.
type sessionManager struct {
	dialer  punchDialer
	tracker *statusTracker
	logger  *Logger

	// trials caps the consecutive punch rounds per connection attempt; 0
	// means defaultTrials.
	trials int

	// punchMu serializes punching: only one upstream connection punches at
	// a time, the rest queue.
	//
	// Serialization is required: one birthday round dials 256 candidate
	// sockets concurrently (Birthday.Candidates defaults to 256) and needs
	// all of them to arm before continuing (waitBirthdayArmed in package
	// burrow returns ErrResourceLimit when armed != expected). Two
	// concurrent punches put 512 sockets in flight, which on Android easily
	// hits the fd limit or candidateSocketBudget (512). Both then fail to
	// arm: not slower, but failing outright.
	//
	// Queue rather than reject: upstream connections often come from a
	// browser, and rejecting means failed page loads; queuing is merely
	// slower, and the next one starts as soon as the previous one connects.
	//
	// It lives here rather than in Client.handle because OpenStream is the
	// single entry point for all punching (called on a lazyConn's first
	// read/write), so serializing here covers every path.
	punchMu sync.Mutex

	// queued is the number of connections waiting to punch, for status
	// display.
	//
	// It must be visible: a queued connection has not entered punch() and
	// never calls setPunchProgress, so without this counter the UI would
	// look stuck with no explanation.
	queued atomic.Int32

	mu     sync.Mutex
	closed bool

	// conns records every punched hole still in use so Close() can close
	// each one.
	//
	// With one hole per connection there is no shared session to close in
	// one go. Without this registry Close() could only close the dialer;
	// punched holes blocked in Read would never be reclaimed and
	// Client.Close()'s wg.Wait() would hang forever.
	conns map[net.Conn]struct{}

	// idle holds pre-punched holes that no connection has taken yet.
	//
	// The only source is background pre-punching (startPool). A second
	// source, returning a connection's hole if it never wrote data, was
	// removed: it misclassified live connections as idle (with a
	// server-speaks-first protocol the forward direction can stay silent
	// for a long time), which showed up as connections dropping right after
	// connecting. Once a hole is taken, the server sets up its forwarding
	// context on the first byte, so handing it to another connection
	// corrupts the protocol. A hole is therefore dedicated once taken and
	// discarded after use.
	//
	// Freshness: the peer never closes an idle hole (the server only
	// declares a hole dead on EOF/RST), but NAT mappings along the path
	// expire silently when idle, so stale holes are still discarded by
	// punchedAt when taken (see idleReuseWindow).
	idle []*idleConn

	// Lifecycle of the background refill goroutine. poolDone is closed by
	// Close(); poolWG waits for the goroutine to actually exit. Otherwise
	// it could finish punching a hole after Close() returns, leaving that
	// hole unowned.
	poolWG        sync.WaitGroup
	poolDone      chan struct{}
	poolCloseOnce sync.Once
}

// idleConn is a punched hole that has never carried data, with the time it
// was punched.
type idleConn struct {
	conn      *punchedConn
	punchedAt time.Time
}

// newSessionManager creates a hole manager. trials caps the punch rounds per
// connection attempt; 0 means defaultTrials.
func newSessionManager(dialer punchDialer, tracker *statusTracker, logger *Logger, trials int) *sessionManager {
	return &sessionManager{
		dialer:   dialer,
		tracker:  tracker,
		logger:   logger,
		trials:   trials,
		conns:    make(map[net.Conn]struct{}),
		poolDone: make(chan struct{}),
	}
}

// OpenStream returns a hole for the caller, punching a new one if no idle
// hole is available.
//
// The name OpenStream is kept so callers need not change (client.handle only
// wants a usable net.Conn), but it now means "get a dedicated hole": there is
// no multiplexing layer on the returned conn, and bytes written to it appear
// unchanged at the peer.
func (m *sessionManager) OpenStream(ctx context.Context) (net.Conn, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errManagerClosed
	}
	m.mu.Unlock()

	// Try an unexpired idle hole first; a hit saves a whole punch.
	if reused := m.takeIdle(); reused != nil {
		m.logger.Infof("reusing idle hole local=%s remote=%s",
			reused.LocalAddr(), reused.RemoteAddr())
		return reused, nil
	}

	return m.openFresh(ctx)
}

// errNoIdleHole reports that the idle pool is currently empty.
//
// The client closes the upstream connection on this error instead of waiting
// for a punch; see takePooled.
var errNoIdleHole = errors.New("tunnel: no idle punched connection available")

// takePooled takes a hole from the idle pool only and never punches inline;
// it returns errNoIdleHole when the pool is empty.
//
// Why the client uses this instead of OpenStream: punching is serialized and
// may run a hundred rounds (seconds to tens of seconds). Holding the upstream
// connection that long shows up as a browser spinning until it times out;
// the user has no idea what is happening, and the upstream application's
// retries are stuck behind the hanging connection. Closing it right away is
// cleaner: the upstream gets "connection refused" and reconnects, while the
// background refill goroutine fills the pool so the next attempt connects
// immediately.
func (m *sessionManager) takePooled() (net.Conn, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errManagerClosed
	}
	m.mu.Unlock()

	if reused := m.takeIdle(); reused != nil {
		m.logger.Infof("taking idle hole local=%s remote=%s",
			reused.LocalAddr(), reused.RemoteAddr())
		return reused, nil
	}
	return nil, errNoIdleHole
}

// openFresh always punches a new hole and never looks at the idle pool.
//
// It must be separate from OpenStream: the background refill (startPool)
// wants one more hole. Going through OpenStream it would take out the hole it
// just added and put it back, so the pool would never grow beyond one no
// matter how large poolSize is.
func (m *sessionManager) openFresh(ctx context.Context) (net.Conn, error) {
	// Queue: only one connection punches at a time (see punchMu).
	//
	// Count first, then take the lock. In the other order, connections
	// waiting on the lock would not be counted and the UI could not show
	// how many are queued.
	waiting := m.queued.Add(1)
	if waiting > 1 {
		// Only report when actually waiting, to avoid log spam without
		// concurrency. The status count includes this connection, matching
		// the "N queued" shown in the UI.
		m.logger.Infof("punch queued, %d ahead", waiting-1)
		m.tracker.setQueued(int(waiting))
	}
	m.punchMu.Lock()
	// Our turn: leave the queue. Publish the remaining count immediately,
	// or the UI would stay at the last non-zero value once the queue
	// drains.
	remaining := m.queued.Add(-1)
	m.tracker.setQueued(int(remaining))
	defer m.punchMu.Unlock()

	// Close() may have run while we waited for the lock. Do not punch then:
	// Close() has already finished its close loop and the new hole would
	// leak.
	m.mu.Lock()
	closedWhileQueued := m.closed
	m.mu.Unlock()
	if closedWhileQueued {
		return nil, errManagerClosed
	}

	conn, err := m.punch(ctx)
	if err != nil {
		return nil, err
	}

	// Registration and the closed check must happen under the same lock.
	// Otherwise Close() could finish its close loop after punch returns but
	// before registration, and nobody would ever close this hole.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		conn.Close()
		m.tracker.sessionDeactivated()
		return nil, errManagerClosed
	}
	m.conns[conn] = struct{}{}
	m.mu.Unlock()

	return &punchedConn{Conn: conn, mgr: m, died: make(chan struct{})}, nil
}

// idleReuseWindow is how long an idle hole may be reused.
//
// The server only declares a hole dead on EOF/RST (see
// zombieCandidateTimeout in server.go), so the peer never closes a hole for
// being idle and this window is not bound by the zombie timeout.
//
// An upper bound is still needed: a hole is a long-lived raw TCP connection,
// and NATs and firewalls on the path silently drop mappings after their own
// idle timeout (commonly 2-5 minutes on home NATs, shorter on mobile
// networks). Once the mapping is gone the hole still looks connected, but
// writes never reach the peer, which is far worse than punching again. 60s
// is well below common NAT idle timeouts yet long enough for the target case
// of pre-punching and then waiting for the user to connect.
const idleReuseWindow = 60 * time.Second

// defaultPoolSize is the number of holes punched ahead of time.
//
// Why 8: the client rejects connections when the pool is empty (see
// takePooled), so the pool size is how many upstream connections can be
// served at once. A browser opens dozens of concurrent connections per page;
// with a pool of 2, a test with 10 concurrent connections served only 2 and
// closed the rest, and much of the page failed to load.
//
// Why not more: each hole costs a goroutine and a socket on the server, and
// punching is serialized (see punchMu) with 256 concurrent candidates per
// round, so warming 8 holes already takes 8 sequential punches. More would
// noticeably delay tunnel readiness and waste resources while idle. Holes
// are refilled as soon as they are used; keeping up in steady state matters
// more than a large initial warm-up.
const defaultPoolSize = 8

// startPool starts the background refill: it fills the idle pool to poolSize
// and replaces holes as they are taken.
//
// A long-running goroutine is used instead of refilling on take because
// refilling means punching, which is serialized (punchMu) and can take tens
// of seconds. Doing it on the take path would hold up the upstream
// connection and cancel out the latency pre-punching is meant to save.
func (m *sessionManager) startPool(poolSize int) {
	if poolSize <= 0 {
		return
	}
	// The refill ctx is canceled by Close() so an in-progress punch aborts
	// immediately.
	//
	// This is required: punch() retries up to trials rounds (default 100),
	// each of which may wait the full AttemptTimeout. The closed flag is
	// only checked between rounds, so Close() could wait for a whole round.
	// Close() waits for this goroutine via poolWG.Wait(), so Client.Close()
	// would hang too. This is the deadlock caught by
	// TestClientCloseRaceWithPendingRegistration.
	ctx, cancel := context.WithCancel(context.Background())
	m.poolWG.Go(func() {
		defer cancel()
		<-m.poolDone
	})
	m.poolWG.Go(func() {
		for {
			// Reap expired holes first, then see how many are missing.
			//
			// Why reap here: expiry used to be checked only in takeIdle(),
			// which runs only when an upstream connection arrives. After the
			// tunnel sat idle longer than idleReuseWindow the pool was full
			// of expired holes, yet the refill loop saw len(m.idle) as full
			// and did nothing. The next upstream connection then found them
			// all expired, closed them and got nil, and since the client
			// rejects on an empty pool, that connection always failed once:
			// leave the app alone for a minute and the next connection
			// fails.
			//
			// Reaping in the refill loop replaces expired holes promptly so
			// the pool stays usable.
			closed, have := m.reapExpired()
			if closed {
				return
			}
			if have >= poolSize {
				select {
				case <-m.poolDone:
					return
				case <-time.After(poolRefillInterval):
				}
				continue
			}

			// Use openFresh, not OpenStream: OpenStream checks the pool
			// first, so refilling through it would take out the hole just
			// added and put it back, leaving the pool stuck at one. Refill
			// needs an unconditional new hole.
			conn, err := m.openFresh(ctx)
			if err != nil {
				// Canceled by Close(): exit now without waiting the
				// interval, since Close() is blocked in poolWG.Wait().
				if ctx.Err() != nil || errors.Is(err, errManagerClosed) {
					return
				}
				// Punch failed (peer offline etc.): wait before retrying.
				// punch() has already retried trials rounds; retrying again
				// right away would only overload the signal server.
				select {
				case <-m.poolDone:
					return
				case <-time.After(poolRefillInterval):
				}
				continue
			}
			pooled, ok := conn.(*punchedConn)
			if !ok {
				conn.Close()
				return
			}
			if !m.putIdle(pooled, time.Now()) {
				// Pool refused it (closed): we still own the hole and
				// must close it.
				pooled.Close()
				return
			}
			m.logger.Infof("pre-punched hole ready, idle pool %d/%d", have+1, poolSize)
		}
	})
}

// poolRefillInterval is how long the refill goroutine waits after finding the
// pool full or after a failed punch.
const poolRefillInterval = 3 * time.Second

// reapExpired closes expired holes in the pool and reports whether the
// manager is closed and how many usable holes remain.
//
// It uses the same expiry rule as takeIdle; the difference is only when it
// runs: the refill loop calls it periodically, independent of upstream
// connections.
func (m *sessionManager) reapExpired() (closed bool, remaining int) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return true, 0
	}
	// Retire holes poolRefillInterval early. The refill loop wakes only
	// every poolRefillInterval; cutting off exactly at idleReuseWindow would
	// let a hole sit nearly expired for up to a full interval, and a
	// connection arriving then would take it (takeIdle treats anything
	// inside the window as usable) and carry data over a hole about to be
	// discarded, which is exactly what reaping is meant to prevent. Retiring
	// early guarantees every pooled hole has at least one refill interval
	// of life left.
	retireAfter := idleReuseWindow - poolRefillInterval
	if retireAfter <= 0 {
		retireAfter = idleReuseWindow
	}
	var expired []*punchedConn
	kept := make([]*idleConn, 0, len(m.idle))
	for _, entry := range m.idle {
		if time.Since(entry.punchedAt) >= retireAfter {
			expired = append(expired, entry.conn)
			continue
		}
		kept = append(kept, entry)
	}
	m.idle = kept
	remaining = len(kept)
	m.mu.Unlock()

	// Close outside the lock: punchedConn.Close() calls release(), which
	// takes the same lock.
	for _, conn := range expired {
		conn.Close()
	}
	if len(expired) > 0 {
		m.logger.Debugf("reaped %d expired idle holes, %d left in pool", len(expired), remaining)
	}
	return false, remaining
}

// putIdle adds a hole that has never carried data to the pool for later
// connections.
//
// The caller must guarantee that not a single byte has been written to the
// hole; otherwise the peer has already forwarded data to the forward target
// and reuse would drop a new request into an old protocol context.
func (m *sessionManager) putIdle(conn *punchedConn, punchedAt time.Time) bool {
	// Past the reuse window: do not keep it.
	if time.Since(punchedAt) >= idleReuseWindow {
		return false
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	m.idle = append(m.idle, &idleConn{conn: conn, punchedAt: punchedAt})
	m.mu.Unlock()
	return true
}

// takeIdle returns an idle hole still inside the reuse window, or nil.
//
// Expired holes are closed and unregistered here rather than skipped: they
// are still registered in conns and would hold an fd and the hole count until
// Close().
func (m *sessionManager) takeIdle() *punchedConn {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	var expired []*punchedConn
	var picked *punchedConn
	// Search newest first: newer holes have more of their window left.
	//
	// Drop only holes that are actually expired. Holes not picked but still
	// inside the window must stay in the pool; they are as good as the one
	// picked. An earlier version closed every unpicked hole as if expired,
	// so each take collapsed the pool to zero: with 2 pre-punched holes the
	// first connection took one and closed the other, the next connection
	// had to punch again, and defaultPoolSize was meaningless.
	kept := make([]*idleConn, 0, len(m.idle))
	for index := len(m.idle) - 1; index >= 0; index-- {
		entry := m.idle[index]
		if time.Since(entry.punchedAt) >= idleReuseWindow {
			expired = append(expired, entry.conn)
			continue
		}
		if picked == nil {
			picked = entry.conn
			continue
		}
		kept = append(kept, entry)
	}
	// kept was built newest to oldest; reverse it back to oldest to newest
	// so the next take still finds the newest hole at the tail.
	slices.Reverse(kept)
	m.idle = kept
	m.mu.Unlock()

	// Close outside the lock: punchedConn.Close() calls release(), which
	// takes the same lock.
	for _, conn := range expired {
		conn.Close()
	}
	return picked
}

// release unregisters a hole after it is closed; the status falls back to
// idle when the hole count reaches zero.
func (m *sessionManager) release(conn net.Conn) {
	m.mu.Lock()
	_, tracked := m.conns[conn]
	delete(m.conns, conn)
	m.mu.Unlock()
	// Decrement only for registered holes. If Close() already removed and
	// closed it, decrementing again would underflow the count and drop the
	// status to idle too early.
	if tracked {
		m.tracker.sessionDeactivated()
	}
}

// punch tries to punch repeatedly until it succeeds, runs out of trials or
// ctx is canceled.
//
// Retrying back to back instead of backing off is required by the birthday
// strategy; see the comment above defaultTrials.
func (m *sessionManager) punch(ctx context.Context) (net.Conn, error) {
	m.tracker.setState(StatePunching)
	room := m.tracker.Snapshot().Room
	m.logger.Infof("punch started room=%s", room)

	startedAt := time.Now()
	var lastErr error

	trials := m.trials
	if trials <= 0 {
		trials = defaultTrials
	}

	for trial := 1; trial <= trials; trial++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return nil, errManagerClosed
		}

		trialStartedAt := time.Now()
		// Report progress in the status, not just the log: a punch may run
		// all 100 rounds, and a static "punching" tells the user nothing
		// about progress or how long to wait.
		m.tracker.setPunchProgress(trial, trials, "")
		m.logger.Infof("punch round %d/%d started (elapsed %s)",
			trial, trials, time.Since(startedAt).Round(time.Second))

		conn, err := m.dialer.Dial(ctx)
		if err == nil {
			local := conn.LocalAddr().String()
			remote := conn.RemoteAddr().String()
			// Use sessionActivated, not setConnected: with one hole per
			// connection the client also has several holes at once, and
			// setConnected would reset the count to 1 and race with other
			// punches. Incrementing matches the server side.
			m.tracker.sessionActivated(local, remote)
			m.logger.Infof("punch succeeded round %d round_time=%s total_time=%s local=%s remote=%s",
				trial, time.Since(trialStartedAt).Round(time.Millisecond),
				time.Since(startedAt).Round(time.Millisecond), local, remote)
			return conn, nil
		}
		lastErr = err

		// Peer offline or signal server rate limiting: an immediate retry
		// is pointless, so wait a short fixed time. Other failures (missed
		// collision, timeout, ...) go straight to the next round, which is
		// how birthday raises its success rate.
		if errors.Is(err, burrow.ErrNoListener) || errors.Is(err, burrow.ErrResourceLimit) {
			reason := "no listener in the target room (check that the server is running)"
			if errors.Is(err, burrow.ErrResourceLimit) {
				reason = "rate limited by the signal server"
			}
			// These reasons are actionable (check whether the server is
			// up), so they must appear in the UI, not just the log.
			m.tracker.setPunchProgress(trial, trials, reason)
			m.logger.Warnf("punch round %d failed: %s, retrying in %s", trial, reason, idleRetryDelay)
			timer := time.NewTimer(idleRetryDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
			continue
		}

		// Misses are normal for birthday (success comes from many rounds),
		// but they must be visible, or the user has no idea what is going
		// on during a long retry run.
		m.logger.Infof("punch round %d missed (took %s): %v, retrying now",
			trial, time.Since(trialStartedAt).Round(time.Millisecond), err)
	}

	m.logger.Warnf("punch failed: no success in %d rounds, last error: %v", trials, lastErr)
	m.tracker.setPunchFailed(lastErr)
	return nil, lastErr
}

// Close stops the refill goroutine and closes every hole, idle or in use.
func (m *sessionManager) Close() error {
	// Set closed before waiting for the refill goroutine: it checks closed
	// on every loop and may be blocked in OpenStream, which returns an error
	// once closed is set, so the loop exits. Waiting first would deadlock.
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.poolCloseOnce.Do(func() { close(m.poolDone) })
	// Wait for the refill goroutine to exit before closing holes; otherwise
	// it could add a new hole to the pool after the close loop and leak it.
	m.poolWG.Wait()

	m.mu.Lock()
	conns := make([]net.Conn, 0, len(m.conns))
	for conn := range m.conns {
		conns = append(conns, conn)
	}
	m.conns = make(map[net.Conn]struct{})
	// Pooled holes are registered in conns too and are covered by the loop
	// above; just empty the pool so nobody takes a closed hole from it.
	m.idle = nil
	m.mu.Unlock()

	// Close each hole: there is no shared session whose closure tears down
	// everything, so every hole is closed individually; otherwise relay
	// goroutines blocked on it would never finish.
	for _, conn := range conns {
		conn.Close()
	}
	return nil
}

// punchedConn is a punched hole that unregisters itself from the
// sessionManager on Close() and broadcasts that the hole is gone.
//
// Holes map one-to-one to upstream connections, so the end of a connection
// means the hole should be closed. The wrapper carries the unregistration so
// the caller (lazyConn) can Close it like any net.Conn.
//
// died is the death signal that replaces smux's GetDieCh(). A hole is now raw
// TCP with no built-in "I am dead" notification, yet closeNotifier
// (relay.go) needs an observation point that works without reading, or
// relayDeferReverse blocks forever. Every path that removes the hole (the
// user's Close, or sessionManager.Close as a fallback) ends in
// punchedConn.Close(), which broadcasts by closing died.
type punchedConn struct {
	net.Conn
	mgr  *sessionManager
	once sync.Once
	died chan struct{}
}

// Close closes the hole, unregisters it and closes died.
func (c *punchedConn) Close() error {
	err := c.Conn.Close()
	// Unregister and broadcast inside once: Close() is called repeatedly
	// (once by lazyConn, once by each relay direction as a fallback).
	// Unregistering twice would over-decrement the hole count and drop the
	// status to idle early, and closing died twice would panic.
	c.once.Do(func() {
		close(c.died)
		c.mgr.release(c.Conn)
	})
	return err
}

// CloseNotify makes the hole itself a closeNotifier so lazyConn can pass the
// signal through.
func (c *punchedConn) CloseNotify() <-chan struct{} { return c.died }

// lazyConn defers punching until the first Read or Write.
//
// It is returned to the upper layer as soon as a TCP connection is accepted,
// so accepting does not wait for a punch, and connections that never send
// data do not consume signaling resources.
//
// Read also triggers the punch: with a server-speaks-first protocol, hooking
// only Write would deadlock.
type lazyConn struct {
	mgr *sessionManager

	once   sync.Once
	stream net.Conn
	err    error

	// punchedAt is when the hole was obtained.
	punchedAt time.Time

	// pooledOnly makes the conn take holes only from the idle pool, failing
	// when it is empty and never punching inline. The client uses it so
	// "no hole available" reaches the upstream immediately instead of
	// making it wait.
	pooledOnly bool

	mu       sync.Mutex
	closed   bool
	closedCh chan struct{} // closed once, for CloseNotify()
	// dialedCh is closed when the punch attempt finishes (success or
	// failure), so CloseNotify() learns that a hole now exists or never
	// will.
	dialedCh chan struct{}
	deadline time.Time
	rdline   time.Time
	wrline   time.Time
}

// newPooledConn creates a conn that uses only pooled holes: ensure() fails
// when the pool is empty, and the caller then closes the upstream connection.
func newPooledConn(mgr *sessionManager) *lazyConn {
	return &lazyConn{
		mgr:        mgr,
		pooledOnly: true,
		closedCh:   make(chan struct{}),
		dialedCh:   make(chan struct{}),
	}
}

// ensure performs the one-time connect and applies any deadlines already set
// to the new hole.
//
// c.stream and c.err are only accessed under c.mu, and c.closed is checked
// again before assignment. "check closed -> connect -> assign" is not
// atomic: without the recheck, Close() could finish while the punch is in
// progress (c.stream still nil, nothing to close), and ensure() would then
// attach the hole to c.stream with nobody left to close it, leaking a
// punched TCP connection and the goroutine blocked on it.
func (c *lazyConn) ensure() (net.Conn, error) {
	c.once.Do(func() {
		// Release CloseNotify waiters on success or failure: after a failed
		// punch this lazyConn is unusable and relayDeferReverse must finish
		// promptly instead of waiting on a channel that never closes.
		defer close(c.dialedCh)

		c.mu.Lock()
		alreadyClosed := c.closed
		c.mu.Unlock()
		if alreadyClosed {
			c.mu.Lock()
			c.err = net.ErrClosed
			c.mu.Unlock()
			return
		}

		var stream net.Conn
		var err error
		if c.pooledOnly {
			// Take a pooled hole or fail immediately; never punch inline
			// and leave the upstream waiting.
			stream, err = c.mgr.takePooled()
		} else {
			stream, err = c.mgr.OpenStream(context.Background())
		}

		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.err = err
			return
		}
		if c.closed {
			// Close() won the race during connect: the hole was never
			// exposed, so it must be closed here or it leaks.
			stream.Close()
			c.err = net.ErrClosed
			return
		}
		c.stream = stream
		c.punchedAt = time.Now()
		if !c.deadline.IsZero() {
			c.stream.SetDeadline(c.deadline)
		}
		if !c.rdline.IsZero() {
			c.stream.SetReadDeadline(c.rdline)
		}
		if !c.wrline.IsZero() {
			c.stream.SetWriteDeadline(c.wrline)
		}
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stream, c.err
}

func (c *lazyConn) Read(b []byte) (int, error) {
	stream, err := c.ensure()
	if err != nil {
		return 0, err
	}
	return stream.Read(b)
}

func (c *lazyConn) Write(b []byte) (int, error) {
	stream, err := c.ensure()
	if err != nil {
		return 0, err
	}
	return stream.Write(b)
}

// There is intentionally no recycle(). An earlier method returned holes that
// had never been written to back to the pool, called by relayDeferReverse
// when the forward copy ended. With a server-speaks-first protocol the
// forward direction can be silent for a long time, so live connections were
// misclassified and their holes returned, showing up as connections dropping
// right after connecting (see relayDeferReverse in relay.go). Fundamentally a
// hole belongs to its connection once taken: the server sets up its
// forwarding context on the first byte, and reusing the hole for another
// connection corrupts the protocol.

// Close does not trigger a punch; an unused conn is simply closed.
func (c *lazyConn) Close() error {
	c.mu.Lock()
	alreadyClosed := c.closed
	c.closed = true
	stream := c.stream
	c.mu.Unlock()
	if !alreadyClosed {
		close(c.closedCh)
	}
	if stream != nil {
		return stream.Close()
	}
	return nil
}

// CloseNotify implements closeNotifier (relay.go) so relayDeferReverse can
// detect that this conn is no longer usable without calling Read.
//
// relayDeferReverse calls it once, before starting the stream->conn
// direction, when the lazyConn has almost always not punched yet (c.stream is
// nil). Then there is no tunnel to lose, and the only thing that makes the
// lazyConn unusable is an explicit Close(), i.e. closedCh being closed. That
// is the most common branch.
//
// As a closeNotifier implementation, though, it must not depend on being
// called before the connect. After the hole exists the underlying connection
// can also die asynchronously (the tunnel drops, or the server closes the
// hole as a zombie candidate) without anyone calling this lazyConn's
// Close(). closedCh alone would miss that and leave the conn->stream
// direction blocked in conn.Read forever, leaking a goroutine and a socket
// (the reason closeNotifier exists in relay.go).
//
// Before smux was removed, "underlying connection died" came from the smux
// stream's GetDieCh(). A hole is now raw TCP with no built-in notification,
// so punchedConn maintains its own died channel (see punchedConn). A probing
// Read must not be used to detect closure: it would race the forward copy
// started later and swallow upper-layer protocol bytes.
//
// Three signals are merged: explicit Close (closedCh), death of the hole
// after the punch (died), and punch failure (dialedCh closed while c.stream
// is still nil, meaning the lazyConn is unusable). A one-shot fan-in
// goroutine merges them and exits as soon as any fires, so it does not leak.
func (c *lazyConn) CloseNotify() <-chan struct{} {
	merged := make(chan struct{})
	go func() {
		defer close(merged)

		// Before the punch the only signal is an explicit Close(); once the
		// punch finishes the hole's own death signal applies, so first wait
		// for the connect attempt to finish.
		select {
		case <-c.closedCh:
			return
		case <-c.dialedCh:
		}

		c.mu.Lock()
		stream := c.stream
		c.mu.Unlock()
		notifier, ok := stream.(closeNotifier)
		if !ok {
			// Punch failed (stream is nil), or the hole type has no death
			// notification (e.g. a fake conn injected by tests). The former
			// notifies immediately; the latter falls back to watching
			// Close().
			if stream == nil {
				return
			}
			<-c.closedCh
			return
		}
		select {
		case <-c.closedCh:
		case <-notifier.CloseNotify():
		}
	}()
	return merged
}

func (c *lazyConn) LocalAddr() net.Addr {
	c.mu.Lock()
	stream := c.stream
	c.mu.Unlock()
	if stream != nil {
		return stream.LocalAddr()
	}
	return placeholderAddr{}
}

func (c *lazyConn) RemoteAddr() net.Addr {
	c.mu.Lock()
	stream := c.stream
	c.mu.Unlock()
	if stream != nil {
		return stream.RemoteAddr()
	}
	return placeholderAddr{}
}

func (c *lazyConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	stream := c.stream
	c.mu.Unlock()
	if stream != nil {
		return stream.SetDeadline(t)
	}
	return nil
}

func (c *lazyConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rdline = t
	stream := c.stream
	c.mu.Unlock()
	if stream != nil {
		return stream.SetReadDeadline(t)
	}
	return nil
}

func (c *lazyConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.wrline = t
	stream := c.stream
	c.mu.Unlock()
	if stream != nil {
		return stream.SetWriteDeadline(t)
	}
	return nil
}

// placeholderAddr stands in for an address before the hole is connected.
type placeholderAddr struct{}

func (placeholderAddr) Network() string { return "burrow" }
func (placeholderAddr) String() string  { return "not connected" }
