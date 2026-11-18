package tunnel

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getkiko/burrow"
)

// Tunnel states.
const (
	StateIdle      = "idle"      // no hole yet (waiting for a connection to trigger punching)
	StatePunching  = "punching"  // punching in progress (birthday runs several rounds)
	StateConnected = "connected" // at least one hole is up and can carry data
	// StatePunchFailed means punchRetries consecutive rounds all failed. It is
	// not a backoff state: a failed punch is retried immediately, and only an
	// exhausted run of rounds ends up here.
	StatePunchFailed = "punch_failed"
)

// Status is a snapshot of the tunnel state.
type Status struct {
	State string
	Room  string

	// The next four fields are only meaningful when State == StateConnected.
	LocalAddr  string        // local ip:port of the punched socket
	RemoteAddr string        // peer ip:port of the punched hole
	PunchedAt  time.Time     // when the hole was established
	Uptime     time.Duration // computed by Snapshot() at call time, not stored

	// SessionCount is the number of holes currently up.
	//
	// Each upstream connection has its own hole (one-to-one, no
	// multiplexing), so this is the number of connections currently running
	// through the tunnel. On the client it equals the number of local
	// connections being forwarded. On the server, birthday fanout may accept
	// several candidates, but only those that have received bytes are counted
	// here; zombie candidates are reaped and never show up.
	//
	// The name stays SessionCount rather than HoleCount because the Android
	// app parses this JSON field by name (TunnelState.kt); renaming it would
	// require a coordinated release on both sides.
	SessionCount int

	// ActiveStreams/TotalStreams largely overlap with the hole count now that
	// multiplexing is gone, but they must not be merged with SessionCount
	// because they count different things:
	//   - SessionCount only counts connections whose hole is up, and on the
	//     server only after the first byte arrives;
	//   - ActiveStreams counts every connection being forwarded, including
	//     client connections still waiting for a hole and server connections
	//     whose forward target is unreachable.
	// A mismatch is itself a useful signal (for example, ActiveStreams staying
	// above SessionCount means upstreams connect but holes do not come up).
	//
	// TotalStreams is cumulative and only increases; it shows how many
	// connections the tunnel has carried in total.
	ActiveStreams int    // connections currently being forwarded
	TotalStreams  uint64 // connections forwarded so far

	Stats *burrow.TCPStats // latest 5-second sample; nil if none yet

	// Peers lists the currently active peer addresses (ip:port).
	//
	// A server can serve several clients, each over its own punched
	// connection. A bare count is not enough: users need to know who is
	// connected (for troubleshooting and to confirm the peer's identity).
	Peers []string

	// Cumulative application-level traffic: bytes actually forwarded through
	// the tunnel.
	//
	// Unlike Stats (kernel TCP_INFO samples), which is tied to a single
	// punched socket and gets mixed up when the server holds several fanout
	// candidates, these counters count the bytes relay actually moved,
	// independent of the number of candidates. They replace Stats for traffic
	// and speed figures on the server.
	//
	// Directions are relative to this end: Rx is received from the tunnel
	// (download), Tx is sent into the tunnel (upload).
	AppRxBytes uint64
	AppTxBytes uint64

	// Punch retry progress: round PunchTrial of PunchTrials. PunchReason is
	// why the current round failed (for example "no listener in the target
	// room"); empty means an ordinary miss.
	//
	// Punching can take a long time (birthday defaults to 100 rounds).
	// Without progress the user only sees "punching" with no idea how long it
	// will take, or whether the peer is offline versus collisions simply
	// missing so far. The two call for completely different actions.
	PunchTrial  int
	PunchTrials int
	PunchReason string

	// Queued is the number of upstream connections waiting for a hole,
	// including the one currently being punched.
	//
	// Punching is serialized (see punchMu in session.go), so concurrent
	// connections take turns. Queued connections have not entered punch() and
	// do not move PunchTrial; without this number the user would see several
	// connections stuck with no indication, indistinguishable from a fault.
	Queued int

	LastError string
}

// Observer receives tunnel state changes. The CLI uses it for logging and the
// Android app uses it to drive the UI.
//
// ActiveStreams changes frequently and is not pushed through callbacks;
// consumers poll Snapshot() instead, which avoids high-frequency callbacks
// across the gomobile JNI boundary.
type Observer interface {
	// OnStateChange delivers a full snapshot on every state transition. A
	// full Status rather than just the state string lets the UI show the
	// addresses the moment a hole is punched, without another poll.
	OnStateChange(status Status)

	// OnStats delivers a TCP quality sample every 5 seconds.
	OnStats(stats burrow.TCPStats)
}

// statusTracker maintains the tunnel state and notifies the Observer on
// transitions. All methods are safe for concurrent use.
type statusTracker struct {
	mu       sync.Mutex
	status   Status
	observer Observer

	// Application traffic uses atomic counters instead of mu: relay calls
	// them on the data path for every chunk, and taking a mutex each time
	// would limit throughput.
	appRx atomic.Uint64
	appTx atomic.Uint64

	// peers is keyed by localAddr: under fanout one client may have several
	// candidates, and the local address distinguishes each active connection.
	peers map[string]string
}

// addAppTraffic adds forwarded application bytes. rx is received from the
// tunnel, tx is sent into it.
func (t *statusTracker) addAppTraffic(rx, tx uint64) {
	if rx > 0 {
		t.appRx.Add(rx)
	}
	if tx > 0 {
		t.appTx.Add(tx)
	}
}

func newStatusTracker(room string, observer Observer) *statusTracker {
	return &statusTracker{
		status:   Status{State: StateIdle, Room: room},
		observer: observer,
		peers:    make(map[string]string),
	}
}

// snapshotLocked builds a snapshot with mu held, deep-copying Stats and
// computing Uptime.
func (t *statusTracker) snapshotLocked() Status {
	snapshot := t.status
	snapshot.AppRxBytes = t.appRx.Load()
	snapshot.AppTxBytes = t.appTx.Load()
	if len(t.peers) > 0 {
		list := make([]string, 0, len(t.peers))
		for _, remote := range t.peers {
			list = append(list, remote)
		}
		sort.Strings(list) // stable order so the UI does not reshuffle on every refresh
		snapshot.Peers = list
	}
	if t.status.Stats != nil {
		statsCopy := *t.status.Stats
		snapshot.Stats = &statsCopy
	}
	if snapshot.State == StateConnected && !snapshot.PunchedAt.IsZero() {
		snapshot.Uptime = time.Since(snapshot.PunchedAt)
	} else {
		snapshot.Uptime = 0
	}
	return snapshot
}

// notify calls the Observer after mu is released; calling external code
// with the lock held risks deadlock.
func (t *statusTracker) notify(snapshot Status) {
	if t.observer != nil {
		t.observer.OnStateChange(snapshot)
	}
}

func (t *statusTracker) setState(state string) {
	t.mu.Lock()
	t.status.State = state
	snapshot := t.snapshotLocked()
	t.mu.Unlock()
	t.notify(snapshot)
}

func (t *statusTracker) setConnected(localAddr, remoteAddr string) {
	t.mu.Lock()
	t.status.State = StateConnected
	t.status.LocalAddr = localAddr
	t.status.RemoteAddr = remoteAddr
	t.status.PunchedAt = time.Now()
	t.status.SessionCount = 1
	t.status.LastError = ""
	snapshot := t.snapshotLocked()
	t.mu.Unlock()
	t.notify(snapshot)
}

// sessionActivated records that a hole became active.
//
// On the server, birthday fanout may accept several candidates and only those
// that received a first byte count as live; on the client, which punches one
// hole per connection, it is called once per punched hole. Both sides use it
// instead of setConnected because setConnected sets SessionCount to 1
// unconditionally, so concurrent holes would overwrite each other's count;
// this method increments it.
//
// The displayed LocalAddr/RemoteAddr/PunchedAt come from the most recently
// activated hole: with several holes only one can be shown, and the newest is
// the most intuitive.
func (t *statusTracker) sessionActivated(localAddr, remoteAddr string) {
	t.mu.Lock()
	t.status.State = StateConnected
	t.status.LocalAddr = localAddr
	t.status.RemoteAddr = remoteAddr
	t.status.PunchedAt = time.Now()
	t.status.SessionCount++
	t.status.LastError = ""
	t.peers[localAddr] = remoteAddr
	snapshot := t.snapshotLocked()
	t.mu.Unlock()
	t.notify(snapshot)
}

// sessionDeactivated records that a previously activated hole ended (normal
// close or zombie reap). When the count reaches zero the state returns to
// idle.
func (t *statusTracker) sessionDeactivated() {
	t.mu.Lock()
	if t.status.SessionCount > 0 {
		t.status.SessionCount--
	}
	if t.status.SessionCount == 0 {
		t.status.State = StateIdle
		t.status.LocalAddr = ""
		t.status.RemoteAddr = ""
		t.status.PunchedAt = time.Time{}
		t.peers = make(map[string]string)
	}
	snapshot := t.snapshotLocked()
	t.mu.Unlock()
	t.notify(snapshot)
}

// setPunchFailed records that a whole run of punch attempts failed. The
// addresses and PunchedAt are cleared so the UI stops showing a dead
// connection.
//
// This is not a backoff: a failed punch is retried immediately (birthday
// relies on many rounds of collisions), and only punchRetries consecutive
// failed rounds end up here.
func (t *statusTracker) setPunchFailed(err error) {
	t.mu.Lock()
	t.status.State = StatePunchFailed
	t.status.LocalAddr = ""
	t.status.RemoteAddr = ""
	t.status.PunchedAt = time.Time{}
	t.status.SessionCount = 0
	if err != nil {
		t.status.LastError = err.Error()
	}
	snapshot := t.snapshotLocked()
	t.mu.Unlock()
	t.notify(snapshot)
}

// setQueued records how many connections are waiting for a hole, including
// the one being punched.
func (t *statusTracker) setQueued(n int) {
	t.mu.Lock()
	t.status.Queued = n
	snapshot := t.snapshotLocked()
	t.mu.Unlock()
	t.notify(snapshot)
}

// setPunchProgress records punch retry progress so the UI can show something
// like "2/100 no server in the target room". An empty reason means an
// ordinary miss that needs no explanation.
func (t *statusTracker) setPunchProgress(trial, trials int, reason string) {
	t.mu.Lock()
	t.status.PunchTrial = trial
	t.status.PunchTrials = trials
	t.status.PunchReason = reason
	snapshot := t.snapshotLocked()
	t.mu.Unlock()
	t.notify(snapshot)
}

// setStats records one TCP sample.
//
// With birthday fanout the server may hold several candidate connections at
// once (up to MaxSuccessCandidates), each pushing its own samples, but only
// one carries data; the rest are zombie candidates with zero traffic.
// Overwriting unconditionally would let their zero samples keep replacing the
// real one, so the displayed speed and traffic would stay near zero.
//
// The filter is LocalAddr+RemoteAddr: only samples matching the currently
// active connection (the one recorded by sessionActivated, which has actually
// received data) are kept; others are dropped. Addresses are used rather than
// comparing cumulative byte counts, because a new connection starts from zero
// and would never win against an older record.
//
// While no connection is active (on the client, or on a server that has not
// received data yet) every sample is accepted.
//
// Known limitation: when a server serves several clients, LocalAddr is the
// most recently activated connection, so RTT, retransmits and similar fields
// reflect only that connection. These are per-connection link metrics that
// cannot be meaningfully summed, and showing one real connection is more
// honest than a synthetic average. Traffic and speed are unaffected; they
// come from the application-level counters (AppRxBytes/AppTxBytes).
func (t *statusTracker) setStats(stats burrow.TCPStats) {
	t.mu.Lock()
	// An empty LocalAddr means no active connection yet: do not filter.
	active := t.status.LocalAddr
	accept := active == "" ||
		(stats.LocalAddr == active && stats.RemoteAddr == t.status.RemoteAddr)
	if accept {
		statsCopy := stats
		t.status.Stats = &statsCopy
	}
	t.mu.Unlock()
	if !accept {
		return
	}
	if t.observer != nil {
		t.observer.OnStats(stats)
	}
}

func (t *statusTracker) streamOpened() {
	t.mu.Lock()
	t.status.ActiveStreams++
	t.status.TotalStreams++
	t.mu.Unlock()
}

func (t *statusTracker) streamClosed() {
	t.mu.Lock()
	if t.status.ActiveStreams > 0 {
		t.status.ActiveStreams--
	}
	t.mu.Unlock()
}

// Snapshot returns a deep copy of the current status; callers may modify it.
func (t *statusTracker) Snapshot() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}
