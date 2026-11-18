package mobile

import (
	"encoding/json"
	"sync"

	"github.com/getkiko/burrow"
	"github.com/getkiko/burrow/tunnel"
)

// Observer receives tunnel events. It is implemented on the Kotlin side.
//
// All parameters are strings: gomobile cannot bind complex types, so status
// and stats are passed as JSON. OnLog takes level as a separate argument
// rather than inside JSON because the UI colors lines by level and should
// not have to parse every line.
//
// These callbacks are synchronous, blocking JNI calls, not asynchronous
// deliveries. Heavy work in an implementation blocks the Go goroutine; UI
// updates should be posted to the main thread.
type Observer interface {
	// OnStateChange delivers a full status snapshot (JSON) on every state transition.
	OnStateChange(statusJSON string)

	// OnStats delivers a TCP sample (JSON) every 5 seconds.
	OnStats(statsJSON string)

	// OnLog delivers a single log line. level is "debug", "info", "warn" or "error".
	OnLog(level string, line string)
}

// peakTracker records peak bandwidth.
//
// It lives in the binding layer rather than in the tunnel package because it
// is purely a UI concern the library should not carry. Samples come from the
// SDK every 5 seconds, so short bursts are smoothed out; that precision is
// accepted in exchange for zero extra overhead.
type peakTracker struct {
	mu      sync.Mutex
	maxDown float64
	maxUp   float64
}

// update folds one sample into the peaks. Samples with a non-positive
// Interval are ignored (avoids division by zero).
func (t *peakTracker) update(stats burrow.TCPStats) {
	seconds := stats.Interval.Seconds()
	if seconds <= 0 {
		return
	}
	down := float64(stats.RxBytes) * 8 / seconds / 1e6
	up := float64(stats.TxBytes) * 8 / seconds / 1e6

	t.mu.Lock()
	defer t.mu.Unlock()
	if down > t.maxDown {
		t.maxDown = down
	}
	if up > t.maxUp {
		t.maxUp = up
	}
}

func (t *peakTracker) peaks() (down, up float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maxDown, t.maxUp
}

func (t *peakTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maxDown = 0
	t.maxUp = 0
}

// statusJSON is the status snapshot sent to Kotlin.
//
// Field names match tunnel.Status (Kotlin parses by these names), with two
// adaptations: durations are converted to milliseconds (Kotlin does not
// understand Go's nanosecond Duration), and the binding layer adds the
// bandwidth peaks.
type statusJSON struct {
	State         string   `json:"State"`
	Peers         []string `json:"Peers"`
	AppRxBytes    uint64   `json:"AppRxBytes"`
	AppTxBytes    uint64   `json:"AppTxBytes"`
	PunchTrial    int      `json:"PunchTrial"`
	PunchTrials   int      `json:"PunchTrials"`
	PunchReason   string   `json:"PunchReason"`
	Queued        int      `json:"Queued"`
	Room          string   `json:"Room"`
	LocalAddr     string   `json:"LocalAddr"`
	RemoteAddr    string   `json:"RemoteAddr"`
	UptimeMillis  int64    `json:"UptimeMillis"`
	SessionCount  int      `json:"SessionCount"`
	ActiveStreams int      `json:"ActiveStreams"`
	TotalStreams  uint64   `json:"TotalStreams"`
	LastError     string   `json:"LastError"`

	MaxDownMbps float64 `json:"MaxDownMbps"`
	MaxUpMbps   float64 `json:"MaxUpMbps"`
}

// encodeStatus encodes a tunnel.Status together with the bandwidth peaks as JSON.
func encodeStatus(status tunnel.Status, maxDown, maxUp float64) string {
	payload := statusJSON{
		State: status.State,
		// A nil slice marshals to null, which Kotlin's List<String> rejects
		// ("Expected start of the array"). Use an empty slice so the output
		// is always an array.
		Peers:         nonNilStrings(status.Peers),
		AppRxBytes:    status.AppRxBytes,
		AppTxBytes:    status.AppTxBytes,
		PunchTrial:    status.PunchTrial,
		PunchTrials:   status.PunchTrials,
		PunchReason:   status.PunchReason,
		Queued:        status.Queued,
		Room:          status.Room,
		LocalAddr:     status.LocalAddr,
		RemoteAddr:    status.RemoteAddr,
		UptimeMillis:  status.Uptime.Milliseconds(),
		SessionCount:  status.SessionCount,
		ActiveStreams: status.ActiveStreams,
		TotalStreams:  status.TotalStreams,
		LastError:     status.LastError,
		MaxDownMbps:   maxDown,
		MaxUpMbps:     maxUp,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// All fields are primitive, so this should never fail; if it does,
		// still give the UI valid JSON.
		return `{"State":"unknown","LastError":"failed to encode status"}`
	}
	return string(data)
}

// encodeStoppedStatus returns the snapshot used when nothing is running.
func encodeStoppedStatus() string {
	return encodeStatus(tunnel.Status{State: "stopped"}, 0, 0)
}

// statsJSON is the TCP sample sent to Kotlin. Durations are in milliseconds.
type statsJSON struct {
	Role       string `json:"Role"`
	LocalAddr  string `json:"LocalAddr"`
	RemoteAddr string `json:"RemoteAddr"`

	IntervalMillis int64  `json:"IntervalMillis"`
	RxBytes        uint64 `json:"RxBytes"`
	TxBytes        uint64 `json:"TxBytes"`
	RetransBytes   uint64 `json:"RetransBytes"`
	TxPackets      uint64 `json:"TxPackets"`
	RetransPackets uint64 `json:"RetransPackets"`
	RxPackets      uint64 `json:"RxPackets"`

	TotalRxBytes      uint64 `json:"TotalRxBytes"`
	TotalTxBytes      uint64 `json:"TotalTxBytes"`
	TotalRetransBytes uint64 `json:"TotalRetransBytes"`

	RTTMillis        int64  `json:"RTTMillis"`
	CongestionWindow uint64 `json:"CongestionWindow"`
	ReceiveWindow    uint64 `json:"ReceiveWindow"`
	ReceiveSpace     uint64 `json:"ReceiveSpace"`
}

// nonNilStrings returns a non-nil slice so the JSON output is [] rather than null.
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func encodeStats(stats burrow.TCPStats) string {
	payload := statsJSON{
		Role:              stats.Role,
		LocalAddr:         stats.LocalAddr,
		RemoteAddr:        stats.RemoteAddr,
		IntervalMillis:    stats.Interval.Milliseconds(),
		RxBytes:           stats.RxBytes,
		TxBytes:           stats.TxBytes,
		RetransBytes:      stats.RetransBytes,
		TxPackets:         stats.TxPackets,
		RetransPackets:    stats.RetransPackets,
		RxPackets:         stats.RxPackets,
		TotalRxBytes:      stats.TotalRxBytes,
		TotalTxBytes:      stats.TotalTxBytes,
		TotalRetransBytes: stats.TotalRetransBytes,
		RTTMillis:         stats.RTT.Milliseconds(),
		CongestionWindow:  stats.CongestionWindow,
		ReceiveWindow:     stats.ReceiveWindow,
		ReceiveSpace:      stats.ReceiveSpace,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return `{"Role":"unknown"}`
	}
	return string(data)
}

// observerBridge adapts the typed tunnel.Observer callbacks to the JSON
// string callbacks of mobile.Observer.
//
// Every callback recovers from panics: the implementation lives on the
// Kotlin side, and a panic there must not take down the Go code that is
// punching or forwarding data.
type observerBridge struct {
	observer Observer
	peaks    *peakTracker
}

func (b *observerBridge) OnStateChange(status tunnel.Status) {
	if b.observer == nil {
		return
	}
	defer silentRecover()
	down, up := b.peaks.peaks()
	b.observer.OnStateChange(encodeStatus(status, down, up))
}

func (b *observerBridge) OnStats(stats burrow.TCPStats) {
	// Update the peaks before the callback: the callback may panic, and the
	// sample must not be lost because of it.
	b.peaks.update(stats)
	if b.observer == nil {
		return
	}
	defer silentRecover()
	b.observer.OnStats(encodeStats(stats))
}

// logSink returns a log callback that can be assigned to a tunnel config.
func (b *observerBridge) logSink() tunnel.LogSink {
	return func(level, line string) {
		if b.observer == nil {
			return
		}
		defer silentRecover()
		b.observer.OnLog(level, line)
	}
}

// Compile-time check that observerBridge implements tunnel.Observer.
var _ tunnel.Observer = (*observerBridge)(nil)
