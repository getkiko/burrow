package tunnel

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// Lines reach an injected sink, filtered by level.
func TestLoggerSinkReceivesLines(t *testing.T) {
	var mu sync.Mutex
	var levels []string
	var lines []string
	sink := func(level, line string) {
		mu.Lock()
		defer mu.Unlock()
		levels = append(levels, level)
		lines = append(lines, line)
	}

	logger := newLoggerWithSink("info", sink)
	logger.Debugf("this one is filtered")
	logger.Infof("info %d", 1)
	logger.Warnf("warning")
	logger.Errorf("error")

	mu.Lock()
	defer mu.Unlock()
	if len(levels) != 3 {
		t.Fatalf("got %d lines, want 3 (debug should be filtered): %v", len(levels), lines)
	}
	for i, want := range []string{"info", "warn", "error"} {
		if levels[i] != want {
			t.Errorf("line %d level = %q, want %q", i, levels[i], want)
		}
	}
	if lines[0] != "info 1" {
		t.Errorf("line 0 = %q, want %q", lines[0], "info 1")
	}
}

// At debug level every line reaches the sink.
func TestLoggerSinkDebugLevel(t *testing.T) {
	var mu sync.Mutex
	var count int
	logger := newLoggerWithSink("debug", func(level, line string) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	logger.Debugf("a")
	logger.Infof("b")
	logger.Warnf("c")
	logger.Errorf("d")

	mu.Lock()
	defer mu.Unlock()
	if count != 4 {
		t.Errorf("got %d lines, want 4", count)
	}
}

// A nil sink must not panic.
func TestLoggerNilSinkDoesNotPanic(t *testing.T) {
	logger := newLoggerWithSink("info", nil)
	logger.Infof("must not panic")
	logger.Errorf("must not panic either")
}

// newLogger keeps working for existing callers.
func TestNewLoggerKeepsWorking(t *testing.T) {
	logger := newLogger("warn")
	if logger == nil {
		t.Fatal("newLogger returned nil")
	}
	logger.Warnf("works")
}

// A panic in the sink must not reach the caller: logging is a side channel
// and must not take down the main flow.
func TestLoggerSinkPanicIsContained(t *testing.T) {
	logger := newLoggerWithSink("info", func(level, line string) {
		panic("deliberate sink panic")
	})
	logger.Infof("trigger sink")
}

// Concurrent logging must be race-free (run with -race).
func TestLoggerSinkConcurrent(t *testing.T) {
	var mu sync.Mutex
	var count int
	logger := newLoggerWithSink("debug", func(level, line string) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { logger.Infof("concurrent") })
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if count != 50 {
		t.Errorf("got %d lines, want 50", count)
	}
}

// Level names are case-insensitive; unknown levels are treated as info.
func TestLoggerLevelParsing(t *testing.T) {
	cases := map[string]bool{ // level -> whether debug lines are delivered
		"debug": true,
		"DEBUG": true,
		"info":  false,
		"":      false,
		"bogus": false,
	}
	for level, wantDebug := range cases {
		t.Run(level, func(t *testing.T) {
			var got bool
			logger := newLoggerWithSink(level, func(l, line string) {
				if l == "debug" {
					got = true
				}
			})
			logger.Debugf("x")
			if got != wantDebug {
				t.Errorf("level=%q: debug delivered = %v, want %v", level, got, wantDebug)
			}
		})
	}
}

// LogSink must not be serialized: json.Marshal fails on func values.
func TestLogSinkNotSerialized(t *testing.T) {
	cfg := ClientConfig{}
	cfg.LogSink = func(level, line string) {}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal ClientConfig failed, LogSink may be missing its json:\"-\" tag: %v", err)
	}
	if strings.Contains(string(data), "LogSink") {
		t.Errorf("LogSink should not appear in the output: %s", data)
	}
}
