package mobile

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/getkiko/burrow/tunnel"
)

// runner abstracts the lifecycle shared by the client and the server.
type runner interface {
	Start() error
	Close() error
	Snapshot() tunnel.Status
}

// Global single instance: Android never needs more than one tunnel at a time.
var (
	mu       sync.Mutex
	current  runner
	starting bool // a Start is in progress (build/Start run without the lock); concurrent Starts are rejected

	// stopRequested is set by Stop() while a start is in progress but
	// current has not been assigned yet. Looking only at current would miss
	// that window (build/Start run outside the lock, so current is still
	// nil): the user taps Stop, Stop() stops nothing, and the tunnel then
	// finishes starting and keeps running in the background. startRunner
	// checks this flag before assigning current and, if it is set, abandons
	// the start and closes the instance it just built.
	stopRequested bool

	peaks = &peakTracker{}
)

var errAlreadyRunning = errors.New("tunnel is already running; call Stop first")

// startRunner is the shared skeleton of StartClient and StartServer: it
// enforces the single instance, builds the concrete runner with build,
// starts it, and rolls back on failure.
//
// The client and server paths are identical apart from the config type and
// constructor; keeping one copy stops the validation and lifecycle logic
// (lock scope, rollback order) from drifting apart. build only turns the
// bridge into a runner; config parsing, validation and error wording stay in
// StartClient/StartServer so a client error is never reported as a server
// one.
//
// Lock scope: build() (tunnel.NewClient/NewServer, which runs the NAT
// self-check on the server side) and instance.Start() may both log, which
// reaches Observer.OnLog through the LogSink. That is a synchronous,
// blocking JNI callback, and calling Snapshot()/Stop() from it is a natural
// thing for the host to do; holding mu at that point would deadlock. So
// build/Start run outside the lock, and a starting flag held under the lock
// guarantees only one Start runs at a time without risking that a rollback
// clears someone else's instance.
//
// While build/Start run outside the lock, current is still nil, so a Stop()
// in that window would see nothing to stop. stopRequested closes the gap:
// Stop() sets it during that window, and startRunner checks it before
// assigning current. If it is set, the start is abandoned and the freshly
// built instance is closed outside the lock instead of being left running
// in the background.
func startRunner(observer Observer, build func(bridge *observerBridge) (runner, error)) error {
	mu.Lock()
	if current != nil {
		mu.Unlock()
		return errAlreadyRunning
	}
	if starting {
		mu.Unlock()
		return errAlreadyRunning
	}
	starting = true
	mu.Unlock()

	// build and Start run entirely outside the lock: both may trigger the
	// synchronous Observer.OnLog callback.
	instance, buildErr := build(&observerBridge{observer: observer, peaks: peaks})
	if buildErr != nil {
		mu.Lock()
		starting = false
		stopRequested = false
		mu.Unlock()
		return buildErr
	}
	if startErr := instance.Start(); startErr != nil {
		mu.Lock()
		starting = false
		stopRequested = false
		mu.Unlock()
		instance.Close() // outside the lock: Close may trigger callbacks
		return startErr
	}

	mu.Lock()
	if stopRequested {
		// Stop() was called during startup: abandon this start; current stays nil.
		stopRequested = false
		starting = false
		mu.Unlock()
		instance.Close() // outside the lock
		return errors.New("start canceled by Stop")
	}
	peaks.reset()
	current = instance
	starting = false
	mu.Unlock()
	return nil
}

// StartClient starts a client from a JSON config. It returns an error if a
// tunnel is already running.
//
// configJSON has the same format as the client config file. cfg.Validate()
// applies exactly the same rules as the tunnel package's file loader
// (LoadClientConfig), so its messages can be shown to the user directly and
// a config that validates never fails to start for validation reasons.
func StartClient(configJSON string, observer Observer) (err error) {
	defer recoverToError(&err)

	var cfg tunnel.ClientConfig
	if unmarshalErr := json.Unmarshal([]byte(configJSON), &cfg); unmarshalErr != nil {
		return errors.New("parse config: " + unmarshalErr.Error())
	}
	if validateErr := cfg.Validate(); validateErr != nil {
		return validateErr
	}

	return startRunner(observer, func(bridge *observerBridge) (runner, error) {
		cfg.LogSink = bridge.logSink()
		client, newErr := tunnel.NewClient(cfg, bridge)
		if newErr != nil {
			return nil, newErr
		}
		return client, nil
	})
}

// StartServer starts a server from a JSON config. It returns an error if a
// tunnel is already running.
//
// It runs a NAT self-check before starting: with the birthday strategy, a
// symmetric NAT (NAT4) makes it refuse to start.
func StartServer(configJSON string, observer Observer) (err error) {
	defer recoverToError(&err)

	var cfg tunnel.ServerConfig
	if unmarshalErr := json.Unmarshal([]byte(configJSON), &cfg); unmarshalErr != nil {
		return errors.New("parse config: " + unmarshalErr.Error())
	}
	if validateErr := cfg.Validate(); validateErr != nil {
		return validateErr
	}

	return startRunner(observer, func(bridge *observerBridge) (runner, error) {
		cfg.LogSink = bridge.logSink()
		server, newErr := tunnel.NewServer(cfg, bridge)
		if newErr != nil {
			return nil, newErr
		}
		return server, nil
	})
}

// Stop stops the current instance. It is a no-op when nothing is running
// and is safe to call repeatedly.
//
// If a Start is running build/Start outside the lock (current is still nil),
// Stop must not conclude there is nothing to stop: the user would think the
// tunnel stopped while it finishes starting and keeps running in the
// background. In that case Stop only sets stopRequested and returns at once
// (it does not wait, to avoid an ANR when called on the Android main
// thread); startRunner abandons the start and cleans up when it finishes.
func Stop() (err error) {
	defer recoverToError(&err)

	mu.Lock()
	instance := current
	current = nil
	if instance == nil && starting {
		stopRequested = true
	}
	mu.Unlock()

	if instance == nil {
		return nil
	}
	// Close outside the lock: Close waits for in-flight connections to wind
	// down and may trigger state callbacks, which are synchronous JNI calls
	// and could deadlock if fired while holding the lock.
	return instance.Close()
}

// Snapshot returns the current status as JSON. When nothing is running it
// returns a snapshot whose State is "stopped".
func Snapshot() (out string) {
	defer recoverToString(&out, encodeStoppedStatus())

	mu.Lock()
	instance := current
	mu.Unlock()

	if instance == nil {
		return encodeStoppedStatus()
	}
	down, up := peaks.peaks()
	return encodeStatus(instance.Snapshot(), down, up)
}
