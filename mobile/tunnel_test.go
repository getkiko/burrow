package mobile

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// cleanup resets global state after each test so tests do not affect each other.
func cleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = Stop() })
}

const validClientJSON = `{
  "listen": "127.0.0.1:0",
  "signal": "ws://127.0.0.1:1/p",
  "room": "test-room",
  "bind_interface": "lo0",
  "strategy": "direct",
  "log_level": "debug"
}`

func TestSnapshotWhenStopped(t *testing.T) {
	cleanup(t)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(Snapshot()), &parsed); err != nil {
		t.Fatalf("Snapshot did not return valid JSON: %v", err)
	}
	if parsed["State"] != "stopped" {
		t.Errorf("State = %v, want stopped", parsed["State"])
	}
}

func TestStopWhenNotRunning(t *testing.T) {
	cleanup(t)
	if err := Stop(); err != nil {
		t.Errorf("Stop with nothing running should return nil, got %v", err)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	cleanup(t)
	for i := range 3 {
		if err := Stop(); err != nil {
			t.Errorf("Stop #%d failed: %v", i, err)
		}
	}
}

// An invalid config returns an error and leaves no running instance.
func TestStartClientRejectsInvalidConfig(t *testing.T) {
	cleanup(t)
	err := StartClient(`{"listen":""}`, &recordingObserver{})
	if err == nil {
		t.Fatal("expected an error for an invalid config")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("error should mention the missing field, got: %v", err)
	}
	var parsed map[string]any
	json.Unmarshal([]byte(Snapshot()), &parsed)
	if parsed["State"] != "stopped" {
		t.Errorf("State after failed start = %v, want stopped", parsed["State"])
	}
}

// Malformed JSON returns an error instead of panicking.
func TestStartClientRejectsMalformedJSON(t *testing.T) {
	cleanup(t)
	if err := StartClient(`{not json`, &recordingObserver{}); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

// A nil observer must not cause a panic.
func TestStartClientNilObserver(t *testing.T) {
	cleanup(t)
	if err := StartClient(`{"listen":""}`, nil); err == nil {
		t.Fatal("expected an error for an invalid config")
	}
}

// A second Start after a successful one returns an explicit error instead
// of silently replacing the running instance.
func TestStartClientRejectsDoubleStart(t *testing.T) {
	cleanup(t)
	obs := &recordingObserver{}
	if err := StartClient(validClientJSON, obs); err != nil {
		t.Skipf("first start failed (environment lacks lo0 or similar), skipping: %v", err)
	}
	err := StartClient(validClientJSON, obs)
	if err == nil {
		t.Fatal("expected an error for a second start")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error should say the tunnel is already running, got: %v", err)
	}
}

// Logs emitted after start reach OnLog: an end-to-end check of the LogSink wiring.
func TestStartClientDeliversLogs(t *testing.T) {
	cleanup(t)
	obs := &recordingObserver{}
	if err := StartClient(validClientJSON, obs); err != nil {
		t.Skipf("start failed (unsupported environment), skipping: %v", err)
	}
	if _, _, logs := obs.counts(); logs == 0 {
		t.Error("expected at least one log line after start")
	}
}

func TestSnapshotAfterStart(t *testing.T) {
	cleanup(t)
	if err := StartClient(validClientJSON, &recordingObserver{}); err != nil {
		t.Skipf("start failed (unsupported environment), skipping: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(Snapshot()), &parsed); err != nil {
		t.Fatalf("Snapshot did not return valid JSON: %v", err)
	}
	if parsed["State"] == "stopped" {
		t.Error("State should not be stopped after start")
	}
}

// At most one concurrent Start succeeds: the core single-instance guarantee.
func TestConcurrentStartOnlyOneSucceeds(t *testing.T) {
	cleanup(t)
	obs := &recordingObserver{}

	var mu sync.Mutex
	var okCount int
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := StartClient(validClientJSON, obs); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if okCount > 1 {
		t.Errorf("%d concurrent Starts succeeded, want at most 1", okCount)
	}
}

// Start works again after Stop.
func TestRestartAfterStop(t *testing.T) {
	cleanup(t)
	obs := &recordingObserver{}
	if err := StartClient(validClientJSON, obs); err != nil {
		t.Skipf("start failed (unsupported environment), skipping: %v", err)
	}
	if err := Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if err := StartClient(validClientJSON, obs); err != nil {
		t.Errorf("restart after Stop failed: %v", err)
	}
}

func TestStartServerRejectsInvalidConfig(t *testing.T) {
	cleanup(t)
	if err := StartServer(`{"forward":""}`, &recordingObserver{}); err == nil {
		t.Fatal("expected an error for an invalid config")
	}
}

// snapshotCallingObserver calls Snapshot() from OnLog, a natural pattern on
// the Kotlin side (refresh the status display on every log line). If
// StartClient fired OnLog while holding the global lock (for example a log
// line inside Start()), this Snapshot() call would try to take the same lock
// again and deadlock.
type snapshotCallingObserver struct {
	recordingObserver
}

func (o *snapshotCallingObserver) OnLog(level, line string) {
	o.recordingObserver.OnLog(level, line)
	Snapshot()
}

// Observer callbacks must never fire while the global lock is held: they are
// synchronous, blocking JNI calls, and a host implementation calling
// Snapshot from them would deadlock. The goroutine plus time.After keeps a
// regression from hanging the test.
func TestObserverCallbackCanCallSnapshot(t *testing.T) {
	cleanup(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		obs := &snapshotCallingObserver{}
		StartClient(validClientJSON, obs)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartClient did not return within 5 seconds; likely a deadlock from a callback fired under the lock")
	}
}

// slowObserver sleeps in OnLog to widen the window in which startRunner runs
// build/Start outside the lock, so tests can call Stop() inside it.
type slowObserver struct {
	recordingObserver
	delay time.Duration
}

func (o *slowObserver) OnLog(level, line string) {
	o.recordingObserver.OnLog(level, line)
	time.Sleep(o.delay)
}

// Stop during startup must not leave a running instance. This is the Android
// "tap Start, then immediately Stop" case: punching and the NAT self-check
// take a while, so Stop lands in startRunner's unlocked phase (current is
// still nil) and must not conclude there is nothing to stop.
func TestStopDuringStartup(t *testing.T) {
	cleanup(t)
	obs := &slowObserver{delay: 300 * time.Millisecond}

	go func() { StartClient(validClientJSON, obs) }()
	time.Sleep(100 * time.Millisecond) // let startup reach the unlocked phase
	Stop()

	// Wait for startup to finish completely (build, Start and any rollback).
	time.Sleep(1 * time.Second)

	var parsed map[string]any
	json.Unmarshal([]byte(Snapshot()), &parsed)
	if parsed["State"] != "stopped" {
		t.Errorf("State after Stop = %v, want stopped; Stop during startup had no effect", parsed["State"])
	}
}
