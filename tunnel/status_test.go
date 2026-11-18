package tunnel

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/getkiko/burrow"
)

// recordingObserver records every callback for assertions.
type recordingObserver struct {
	mu     sync.Mutex
	states []Status
	stats  []burrow.TCPStats
}

func (o *recordingObserver) OnStateChange(s Status) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states = append(o.states, s)
}

func (o *recordingObserver) OnStats(s burrow.TCPStats) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stats = append(o.stats, s)
}

func (o *recordingObserver) stateCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.states)
}

func (o *recordingObserver) lastState() Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.states) == 0 {
		return Status{}
	}
	return o.states[len(o.states)-1]
}

func TestStatusTrackerInitialState(t *testing.T) {
	tr := newStatusTracker("my-room", nil)
	got := tr.Snapshot()
	if got.State != StateIdle {
		t.Errorf("State = %q, want %q", got.State, StateIdle)
	}
	if got.Room != "my-room" {
		t.Errorf("Room = %q", got.Room)
	}
	if got.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", got.SessionCount)
	}
}

func TestStatusTrackerSetConnected(t *testing.T) {
	obs := &recordingObserver{}
	tr := newStatusTracker("r", obs)

	tr.setState(StatePunching)
	tr.setConnected("192.168.1.5:41230", "1.2.3.4:54321")

	got := tr.Snapshot()
	if got.State != StateConnected {
		t.Errorf("State = %q", got.State)
	}
	if got.LocalAddr != "192.168.1.5:41230" {
		t.Errorf("LocalAddr = %q", got.LocalAddr)
	}
	if got.RemoteAddr != "1.2.3.4:54321" {
		t.Errorf("RemoteAddr = %q", got.RemoteAddr)
	}
	if got.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1", got.SessionCount)
	}
	if got.PunchedAt.IsZero() {
		t.Error("PunchedAt should be set")
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
	// one notification per state transition
	if obs.stateCount() != 2 {
		t.Errorf("OnStateChange called %d times, want 2", obs.stateCount())
	}
	if obs.lastState().State != StateConnected {
		t.Errorf("last notified State = %q", obs.lastState().State)
	}
}

func TestStatusTrackerUptime(t *testing.T) {
	tr := newStatusTracker("r", nil)
	tr.setConnected("l", "r")
	time.Sleep(20 * time.Millisecond)

	got := tr.Snapshot()
	if got.Uptime <= 0 {
		t.Errorf("Uptime = %v, want > 0", got.Uptime)
	}
	if got.Uptime > time.Second {
		t.Errorf("Uptime = %v, too large", got.Uptime)
	}
}

// Uptime is always 0 while not connected.
func TestStatusTrackerUptimeZeroWhenIdle(t *testing.T) {
	tr := newStatusTracker("r", nil)
	if got := tr.Snapshot(); got.Uptime != 0 {
		t.Errorf("Uptime = %v, want 0", got.Uptime)
	}
}

func TestStatusTrackerPunchFailed(t *testing.T) {
	tr := newStatusTracker("r", nil)
	tr.setPunchFailed(errors.New("no listener in target room"))

	got := tr.Snapshot()
	if got.State != StatePunchFailed {
		t.Errorf("State = %q", got.State)
	}
	if got.LastError != "no listener in target room" {
		t.Errorf("LastError = %q", got.LastError)
	}
	if got.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", got.SessionCount)
	}
}

// Going from connected to punch failed must clear the addresses so the UI
// does not show stale data.
func TestStatusTrackerPunchFailedClearsAddrs(t *testing.T) {
	tr := newStatusTracker("r", nil)
	tr.setConnected("192.168.1.5:41230", "1.2.3.4:54321")
	tr.setPunchFailed(errors.New("broken"))

	got := tr.Snapshot()
	if got.LocalAddr != "" || got.RemoteAddr != "" {
		t.Errorf("addresses not cleared: local=%q remote=%q", got.LocalAddr, got.RemoteAddr)
	}
	if !got.PunchedAt.IsZero() {
		t.Error("PunchedAt should be zero")
	}
}

func TestStatusTrackerStreamCounting(t *testing.T) {
	tr := newStatusTracker("r", nil)

	tr.streamOpened()
	tr.streamOpened()
	tr.streamOpened()
	if got := tr.Snapshot(); got.ActiveStreams != 3 || got.TotalStreams != 3 {
		t.Errorf("Active = %d, Total = %d, want 3/3", got.ActiveStreams, got.TotalStreams)
	}

	tr.streamClosed()
	if got := tr.Snapshot(); got.ActiveStreams != 2 || got.TotalStreams != 3 {
		t.Errorf("Active = %d, Total = %d, want 2/3", got.ActiveStreams, got.TotalStreams)
	}
}

func TestStatusTrackerSetStats(t *testing.T) {
	obs := &recordingObserver{}
	tr := newStatusTracker("r", obs)
	stats := burrow.TCPStats{RTT: 48 * time.Millisecond, TxBytes: 1000}

	tr.setStats(stats)

	got := tr.Snapshot()
	if got.Stats == nil {
		t.Fatal("Stats should not be nil")
	}
	if got.Stats.RTT != 48*time.Millisecond {
		t.Errorf("Stats.RTT = %v", got.Stats.RTT)
	}
	obs.mu.Lock()
	statsCount := len(obs.stats)
	obs.mu.Unlock()
	if statsCount != 1 {
		t.Errorf("OnStats called %d times, want 1", statsCount)
	}
}

// Snapshot must return a deep copy: modifying the result must not affect
// internal state.
func TestStatusTrackerSnapshotIsCopy(t *testing.T) {
	tr := newStatusTracker("r", nil)
	tr.setStats(burrow.TCPStats{RTT: 10 * time.Millisecond})

	first := tr.Snapshot()
	first.State = "tampered"
	first.Stats.RTT = 999 * time.Second

	second := tr.Snapshot()
	if second.State == "tampered" {
		t.Error("Snapshot did not isolate State")
	}
	if second.Stats.RTT != 10*time.Millisecond {
		t.Errorf("Snapshot did not deep-copy Stats, RTT = %v", second.Stats.RTT)
	}
}

func TestStatusTrackerConcurrentAccess(t *testing.T) {
	tr := newStatusTracker("r", nil)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			tr.streamOpened()
			tr.setStats(burrow.TCPStats{TxBytes: 1})
			_ = tr.Snapshot()
			tr.streamClosed()
		})
	}
	wg.Wait()
	if got := tr.Snapshot(); got.ActiveStreams != 0 {
		t.Errorf("ActiveStreams = %d, want 0", got.ActiveStreams)
	}
	if got := tr.Snapshot(); got.TotalStreams != 50 {
		t.Errorf("TotalStreams = %d, want 50", got.TotalStreams)
	}
}

// sessionActivated increments the count instead of overwriting it with 1 like
// setConnected; the server relies on this when several fanout candidates
// activate concurrently.
func TestStatusTrackerSessionActivatedIncrements(t *testing.T) {
	tr := newStatusTracker("r", nil)

	tr.sessionActivated("l1", "r1")
	if got := tr.Snapshot(); got.SessionCount != 1 || got.State != StateConnected {
		t.Fatalf("after first activation SessionCount=%d State=%q, want 1/%q", got.SessionCount, got.State, StateConnected)
	}

	tr.sessionActivated("l2", "r2")
	got := tr.Snapshot()
	if got.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2 (both candidates active)", got.SessionCount)
	}
	if got.LocalAddr != "l2" || got.RemoteAddr != "r2" {
		t.Errorf("displayed addresses should be the latest activated candidate, got local=%q remote=%q", got.LocalAddr, got.RemoteAddr)
	}
}

// sessionDeactivated decrements the count; at zero the state returns to idle
// and the displayed addresses are cleared.
func TestStatusTrackerSessionDeactivated(t *testing.T) {
	tr := newStatusTracker("r", nil)
	tr.sessionActivated("l1", "r1")
	tr.sessionActivated("l2", "r2")

	tr.sessionDeactivated()
	if got := tr.Snapshot(); got.SessionCount != 1 || got.State != StateConnected {
		t.Errorf("with 1 active left SessionCount=%d State=%q, want 1/%q", got.SessionCount, got.State, StateConnected)
	}

	tr.sessionDeactivated()
	got := tr.Snapshot()
	if got.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", got.SessionCount)
	}
	if got.State != StateIdle {
		t.Errorf("after all candidates deactivated State = %q, want %q", got.State, StateIdle)
	}
	if got.LocalAddr != "" || got.RemoteAddr != "" {
		t.Errorf("displayed addresses should be cleared, got local=%q remote=%q", got.LocalAddr, got.RemoteAddr)
	}
}

// Extra sessionDeactivated calls must not drive SessionCount negative.
func TestStatusTrackerSessionDeactivatedNoUnderflow(t *testing.T) {
	tr := newStatusTracker("r", nil)
	tr.sessionDeactivated()
	if got := tr.Snapshot().SessionCount; got != 0 {
		t.Errorf("SessionCount = %d, want 0 (no underflow)", got)
	}
}

// A nil observer must not panic.
func TestStatusTrackerNilObserver(t *testing.T) {
	tr := newStatusTracker("r", nil)
	tr.setState(StatePunching)
	tr.setConnected("l", "r")
	tr.setStats(burrow.TCPStats{})
	tr.setPunchFailed(errors.New("x"))
	tr.sessionActivated("l", "r")
	tr.sessionDeactivated()
}
