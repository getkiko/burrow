package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getkiko/burrow/internal/birthdaylab"
)

const maxAPIRunLogBytes = 4 << 20

type apiRunView struct {
	ID         string     `json:"id"`
	Command    string     `json:"command"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Log        string     `json:"log,omitempty"`
	Cursor     int64      `json:"cursor"`
}

type apiRun struct {
	mu              sync.Mutex
	id              string
	command         string
	status          string
	startedAt       time.Time
	finishedAt      *time.Time
	errorMessage    string
	logData         []byte
	logBase         int64
	notify          chan struct{}
	cancel          context.CancelFunc
	cancelRequested bool
}

type apiRunManager struct {
	mu   sync.RWMutex
	runs map[string]*apiRun
}

type apiRunWriter struct {
	run *apiRun
}

func newAPIRunManager() *apiRunManager {
	return &apiRunManager{runs: make(map[string]*apiRun)}
}

func (manager *apiRunManager) start(spec apiCommandSpec, values map[string]any) (apiRunView, error) {
	id, err := newAPIRunID()
	if err != nil {
		return apiRunView{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &apiRun{
		id:        id,
		command:   spec.Command,
		status:    "running",
		startedAt: time.Now(),
		notify:    make(chan struct{}),
		cancel:    cancel,
	}
	manager.mu.Lock()
	manager.pruneLocked()
	manager.runs[id] = run
	manager.mu.Unlock()

	writer := apiRunWriter{run: run}
	fmt.Fprintf(writer, "[run] %s\n", spec.Command)
	go func() {
		err := executeIntegratedCommand(ctx, spec, values, writer)
		run.finish(err)
	}()
	return run.snapshot(true), nil
}

func (manager *apiRunManager) get(id string) (*apiRun, bool) {
	manager.mu.RLock()
	run, exists := manager.runs[id]
	manager.mu.RUnlock()
	return run, exists
}

func (manager *apiRunManager) list() []apiRunView {
	manager.mu.RLock()
	runs := make([]*apiRun, 0, len(manager.runs))
	for _, run := range manager.runs {
		runs = append(runs, run)
	}
	manager.mu.RUnlock()
	views := make([]apiRunView, 0, len(runs))
	for _, run := range runs {
		views = append(views, run.snapshot(false))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].StartedAt.After(views[j].StartedAt) })
	return views
}

func (manager *apiRunManager) stop(id string) (apiRunView, error) {
	run, exists := manager.get(id)
	if !exists {
		return apiRunView{}, fmt.Errorf("run %s not found", id)
	}
	run.mu.Lock()
	if run.status == "running" {
		run.cancelRequested = true
		run.status = "stopping"
		run.signalUpdateLocked()
		run.cancel()
	}
	view := run.viewLocked(true)
	run.mu.Unlock()
	return view, nil
}

func (manager *apiRunManager) stopAll() {
	manager.mu.RLock()
	runs := make([]*apiRun, 0, len(manager.runs))
	for _, run := range manager.runs {
		runs = append(runs, run)
	}
	manager.mu.RUnlock()
	for _, run := range runs {
		run.mu.Lock()
		if run.status == "running" || run.status == "stopping" {
			run.cancelRequested = true
			run.cancel()
		}
		run.mu.Unlock()
	}
}

func (manager *apiRunManager) pruneLocked() {
	if len(manager.runs) < 100 {
		return
	}
	var completed []*apiRun
	for _, run := range manager.runs {
		view := run.snapshot(false)
		if isTerminalRunStatus(view.Status) {
			completed = append(completed, run)
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].startedAt.Before(completed[j].startedAt) })
	for len(manager.runs) >= 100 && len(completed) > 0 {
		delete(manager.runs, completed[0].id)
		completed = completed[1:]
	}
}

func (run *apiRun) finish(err error) {
	run.cancel()
	run.mu.Lock()
	now := time.Now()
	run.finishedAt = &now
	switch {
	case run.cancelRequested || errors.Is(err, context.Canceled):
		run.status = "canceled"
	case err != nil:
		run.status = "failed"
		run.errorMessage = err.Error()
		run.appendOutputLocked([]byte(fmt.Sprintf("[error] %v\n", err)))
	default:
		run.status = "succeeded"
	}
	run.signalUpdateLocked()
	run.mu.Unlock()
}

func (run *apiRun) appendOutput(data []byte) {
	run.mu.Lock()
	run.appendOutputLocked(data)
	run.signalUpdateLocked()
	run.mu.Unlock()
}

func (run *apiRun) appendOutputLocked(data []byte) {
	run.logData = append(run.logData, data...)
	if overflow := len(run.logData) - maxAPIRunLogBytes; overflow > 0 {
		run.logData = append([]byte(nil), run.logData[overflow:]...)
		run.logBase += int64(overflow)
	}
}

func (run *apiRun) signalUpdateLocked() {
	close(run.notify)
	run.notify = make(chan struct{})
}

func (run *apiRun) snapshot(includeLog bool) apiRunView {
	run.mu.Lock()
	view := run.viewLocked(includeLog)
	run.mu.Unlock()
	return view
}

func (run *apiRun) viewLocked(includeLog bool) apiRunView {
	view := apiRunView{
		ID:         run.id,
		Command:    run.command,
		Status:     run.status,
		StartedAt:  run.startedAt,
		FinishedAt: run.finishedAt,
		Error:      run.errorMessage,
		Cursor:     run.logBase + int64(len(run.logData)),
	}
	if includeLog {
		view.Log = string(run.logData)
	}
	return view
}

func (run *apiRun) readFrom(cursor int64) (apiRunView, string, int64, <-chan struct{}) {
	run.mu.Lock()
	if cursor < run.logBase {
		cursor = run.logBase
	}
	end := run.logBase + int64(len(run.logData))
	if cursor > end {
		cursor = end
	}
	chunk := string(run.logData[cursor-run.logBase:])
	view := run.viewLocked(false)
	notify := run.notify
	run.mu.Unlock()
	return view, chunk, end, notify
}

func (writer apiRunWriter) Write(data []byte) (int, error) {
	copyOfData := append([]byte(nil), data...)
	writer.run.appendOutput(copyOfData)
	return len(data), nil
}

func newAPIRunID() (string, error) {
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func isTerminalRunStatus(status string) bool {
	return status == "succeeded" || status == "failed" || status == "canceled"
}

func executeIntegratedCommand(ctx context.Context, spec apiCommandSpec, values map[string]any, output io.Writer) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("run panicked: %v", recovered)
		}
	}()
	switch spec.ID {
	case "tcpnat":
		return runTCPNAT(ctx, output, tcpNATConfig{
			server:     stringValue(values, "server"),
			signalPath: stringValue(values, "signal_path"),
			ports:      stringValue(values, "ports"),
			iface:      stringValue(values, "iface"),
			localPort:  intValue(values, "local-port"),
			rounds:     intValue(values, "rounds"),
			timeout:    durationValue(values, "timeout"),
		})
	case "stun":
		return runSTUN(ctx, output, stunConfig{
			server:  stringValue(values, "stun"),
			iface:   stringValue(values, "iface"),
			timeout: durationValue(values, "timeout"),
		})
	case "punch/client":
		return runPunchClient(ctx, output, punchClientConfig{
			signal:     stringValue(values, "signal"),
			signalPath: stringValue(values, "signal_path"),
			room:       stringValue(values, "room"),
			message:    stringValue(values, "msg"),
			iface:      stringValue(values, "iface"),
			bench:      boolValue(values, "bench"),
			size:       stringValue(values, "size"),
		})
	case "punch/server":
		return runPunchServer(ctx, output, punchServerConfig{
			signal:     stringValue(values, "signal"),
			signalPath: stringValue(values, "signal_path"),
			room:       stringValue(values, "room"),
			iface:      stringValue(values, "iface"),
			bench:      boolValue(values, "bench"),
		})
	case "birthday/client":
		return runBirthdayClient(ctx, output, birthdayClientConfig{
			signalAddress:  stringValue(values, "signal"),
			signalPath:     stringValue(values, "signal_path"),
			room:           stringValue(values, "room"),
			iface:          stringValue(values, "iface"),
			candidates:     intValue(values, "candidates"),
			trials:         intValue(values, "trials"),
			minPort:        intValue(values, "min-port"),
			maxPort:        intValue(values, "max-port"),
			warmup:         durationValue(values, "warmup"),
			attemptTimeout: durationValue(values, "attempt-timeout"),
			pairTimeout:    durationValue(values, "pair-timeout"),
		})
	case "birthday/server":
		signalAddress, probeSignals, err := apiBirthdayServerSignals(values)
		if err != nil {
			return err
		}
		return runBirthdayServer(ctx, output, output, birthdayServerConfig{
			signalAddress: signalAddress,
			signalPath:    stringValue(values, "signal_path"),
			probeSignals:  probeSignals,
			probePorts:    stringValue(values, "probe-ports"),
			room:          stringValue(values, "room"),
			iface:         stringValue(values, "iface"),
			mode:          stringValue(values, "mode"),
			localPort:     intValue(values, "local-port"),
			publicAddress: stringValue(values, "public-address"),
			trials:        intValue(values, "trials"),
		})
	default:
		return fmt.Errorf("unknown command %q", spec.ID)
	}
}

func apiBirthdayServerSignals(values map[string]any) (primary, probes string, err error) {
	addresses, err := birthdaylab.ParseSignalAddresses(stringValue(values, "signal"))
	if err != nil {
		return "", "", err
	}
	primary = addresses[0]
	if len(addresses) > 1 {
		probes = strings.Join(addresses, "\n")
	}
	return primary, probes, nil
}

func durationValue(values map[string]any, name string) time.Duration {
	value, _ := values[name].(string)
	duration, _ := time.ParseDuration(value)
	return duration
}
