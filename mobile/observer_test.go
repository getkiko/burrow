package mobile

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/getkiko/burrow"
	"github.com/getkiko/burrow/tunnel"
)

// recordingObserver records received callbacks for assertions.
type recordingObserver struct {
	mu     sync.Mutex
	states []string
	stats  []string
	logs   []string
}

func (o *recordingObserver) OnStateChange(statusJSON string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states = append(o.states, statusJSON)
}

func (o *recordingObserver) OnStats(statsJSON string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stats = append(o.stats, statsJSON)
}

func (o *recordingObserver) OnLog(level, line string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.logs = append(o.logs, level+"|"+line)
}

func (o *recordingObserver) counts() (states, stats, logs int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.states), len(o.stats), len(o.logs)
}

// Status must encode to JSON Kotlin can parse, with durations in milliseconds
// and bandwidth peaks added by the binding layer.
func TestEncodeStatus(t *testing.T) {
	status := tunnel.Status{
		State:         "connected",
		Room:          "r1",
		LocalAddr:     "192.168.1.5:41230",
		RemoteAddr:    "1.2.3.4:54321",
		Uptime:        90 * time.Second,
		SessionCount:  1,
		ActiveStreams: 7,
		TotalStreams:  243,
	}
	got := encodeStatus(status, 8.1, 1.2)

	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v (raw %q)", err, got)
	}
	if parsed["State"] != "connected" {
		t.Errorf("State = %v", parsed["State"])
	}
	if parsed["ActiveStreams"] != float64(7) {
		t.Errorf("ActiveStreams = %v, want 7", parsed["ActiveStreams"])
	}
	if parsed["UptimeMillis"] != float64(90000) {
		t.Errorf("UptimeMillis = %v, want 90000", parsed["UptimeMillis"])
	}
	if parsed["MaxDownMbps"] != 8.1 {
		t.Errorf("MaxDownMbps = %v, want 8.1", parsed["MaxDownMbps"])
	}
	if parsed["MaxUpMbps"] != 1.2 {
		t.Errorf("MaxUpMbps = %v, want 1.2", parsed["MaxUpMbps"])
	}
}

func TestEncodeStoppedStatus(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(encodeStoppedStatus()), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["State"] != "stopped" {
		t.Errorf("State = %v, want stopped", parsed["State"])
	}
}

func TestEncodeStats(t *testing.T) {
	stats := burrow.TCPStats{
		Role:       "dialer",
		LocalAddr:  "l",
		RemoteAddr: "r",
		Interval:   5 * time.Second,
		RxBytes:    2_000_000,
		TxBytes:    250_000,
		RTT:        48 * time.Millisecond,
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(encodeStats(stats)), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["Role"] != "dialer" {
		t.Errorf("Role = %v", parsed["Role"])
	}
	if parsed["RTTMillis"] != float64(48) {
		t.Errorf("RTTMillis = %v, want 48", parsed["RTTMillis"])
	}
}

// Bandwidth peaks only grow; reset clears them.
func TestBandwidthPeakTracking(t *testing.T) {
	tr := &peakTracker{}

	// 2 MB in 5 s => 3.2 Mbps
	tr.update(burrow.TCPStats{Interval: 5 * time.Second, RxBytes: 2_000_000, TxBytes: 250_000})
	down, up := tr.peaks()
	if down < 3.1 || down > 3.3 {
		t.Errorf("download peak = %.2f, want about 3.2", down)
	}
	if up < 0.35 || up > 0.45 {
		t.Errorf("upload peak = %.2f, want about 0.4", up)
	}

	tr.update(burrow.TCPStats{Interval: 5 * time.Second, RxBytes: 100_000, TxBytes: 10_000})
	if down2, _ := tr.peaks(); down2 != down {
		t.Errorf("smaller sample changed the peak to %.2f, want %.2f", down2, down)
	}

	tr.update(burrow.TCPStats{Interval: 5 * time.Second, RxBytes: 10_000_000, TxBytes: 20_000})
	if down3, _ := tr.peaks(); down3 <= down {
		t.Errorf("larger sample did not raise the peak: %.2f -> %.2f", down, down3)
	}

	tr.reset()
	if d, u := tr.peaks(); d != 0 || u != 0 {
		t.Errorf("after reset = (%.2f, %.2f), want (0, 0)", d, u)
	}
}

// A zero Interval must not divide by zero.
func TestBandwidthZeroInterval(t *testing.T) {
	tr := &peakTracker{}
	tr.update(burrow.TCPStats{Interval: 0, RxBytes: 1000})
	if d, u := tr.peaks(); d != 0 || u != 0 {
		t.Errorf("sample with Interval=0 should be ignored, got (%.2f, %.2f)", d, u)
	}
}

func TestPeakTrackerConcurrent(t *testing.T) {
	tr := &peakTracker{}
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			tr.update(burrow.TCPStats{Interval: time.Second, RxBytes: 1_000_000})
			tr.peaks()
		})
	}
	wg.Wait()
}

// The bridge turns typed callbacks into JSON string callbacks.
func TestObserverBridge(t *testing.T) {
	obs := &recordingObserver{}
	bridge := &observerBridge{observer: obs, peaks: &peakTracker{}}

	bridge.OnStateChange(tunnel.Status{State: "connected"})
	bridge.OnStats(burrow.TCPStats{Interval: 5 * time.Second, RxBytes: 1_000_000})

	states, stats, _ := obs.counts()
	if states != 1 {
		t.Fatalf("got %d state callbacks, want 1", states)
	}
	if stats != 1 {
		t.Fatalf("got %d stats callbacks, want 1", stats)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(obs.states[0]), &parsed); err != nil {
		t.Errorf("state callback is not valid JSON: %v", err)
	}
}

// A nil observer must not panic (Kotlin may pass null).
func TestObserverBridgeNilObserver(t *testing.T) {
	bridge := &observerBridge{observer: nil, peaks: &peakTracker{}}
	bridge.OnStateChange(tunnel.Status{State: "x"})
	bridge.OnStats(burrow.TCPStats{})
	bridge.logSink()("info", "x")
}

// A panicking Kotlin callback must not propagate into the Go code.
func TestObserverBridgeContainsPanic(t *testing.T) {
	bridge := &observerBridge{observer: panickingObserver{}, peaks: &peakTracker{}}
	bridge.OnStateChange(tunnel.Status{State: "x"})
	bridge.OnStats(burrow.TCPStats{})
	bridge.logSink()("info", "x")
}

type panickingObserver struct{}

func (panickingObserver) OnStateChange(string) { panic("intentional panic") }
func (panickingObserver) OnStats(string)       { panic("intentional panic") }
func (panickingObserver) OnLog(string, string) { panic("intentional panic") }

// Samples still count toward the peaks when the observer panics, because
// the peaks are updated before the callback.
func TestPeakUpdatedEvenIfObserverPanics(t *testing.T) {
	tr := &peakTracker{}
	bridge := &observerBridge{observer: panickingObserver{}, peaks: tr}
	bridge.OnStats(burrow.TCPStats{Interval: 5 * time.Second, RxBytes: 2_000_000})
	if down, _ := tr.peaks(); down <= 0 {
		t.Errorf("peak = %.2f, want > 0; a panicking callback must not drop the sample", down)
	}
}
