package burrow

import (
	"context"
	"errors"
	"net"
	"runtime"
	"testing"
	"time"
)

func testDialerConfig() Config {
	return Config{
		SignalAddr:   "127.0.0.1:1", // unreachable, so Dial stays in the signaling phase
		Room:         "room",
		PairTimeout:  30 * time.Second,
		PunchTimeout: 30 * time.Second,
	}
}

func TestConfigAddressFamily(t *testing.T) {
	t.Run("default IPv4", func(t *testing.T) {
		cfg, err := testDialerConfig().normalize()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AddressFamily != AddressFamilyIPv4 || cfg.tcpNetwork() != "tcp4" {
			t.Fatalf("default address family = %q / %q", cfg.AddressFamily, cfg.tcpNetwork())
		}
	})

	t.Run("explicit IPv6", func(t *testing.T) {
		input := testDialerConfig()
		input.AddressFamily = AddressFamilyIPv6
		cfg, err := input.normalize()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AddressFamily != AddressFamilyIPv6 || cfg.tcpNetwork() != "tcp6" {
			t.Fatalf("IPv6 address family = %q / %q", cfg.AddressFamily, cfg.tcpNetwork())
		}
	})

	t.Run("invalid", func(t *testing.T) {
		input := testDialerConfig()
		input.AddressFamily = "auto"
		if _, err := input.normalize(); err == nil {
			t.Fatal("invalid address family should return an error")
		}
	})
}

// Close must abort an in-flight Dial instead of letting it run until timeout.
//
// The signal server here accepts connections but never responds. With an
// unreachable address, Dial would fail immediately with connection refused,
// never be in flight, and the test would prove nothing.
func TestDialerCloseAbortsInflightDial(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and ignore, so the client waits for a signaling response.
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	cfg := testDialerConfig()
	cfg.SignalAddr = ln.Addr().String()
	d, err := NewDialer(cfg)
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := d.Dial(context.Background())
		errCh <- err
	}()

	// Make sure Dial is still running (did not fail early), or the test is meaningless.
	select {
	case err := <-errCh:
		t.Fatalf("Dial returned early, never in flight: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	start := time.Now()
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closeElapsed := time.Since(start)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("in-flight Dial should fail after Close")
		}
		t.Logf("in-flight Dial returned: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not abort the in-flight Dial")
	}

	// Close's wg.Wait waits for the in-flight Dial, but not for the full PairTimeout.
	if closeElapsed > 5*time.Second {
		t.Errorf("Close took too long (%v); the in-flight Dial was not aborted", closeElapsed)
	}
}

// Dial after Close must return ErrClosed immediately.
func TestDialerDialAfterClose(t *testing.T) {
	d, err := NewDialer(testDialerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = d.Dial(context.Background())
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Dial after Close should return immediately, took %v", elapsed)
	}
}

// Close is idempotent.
func TestDialerCloseIdempotent(t *testing.T) {
	d, err := NewDialer(testDialerConfig())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := d.Close(); err != nil {
			t.Fatalf("repeated Close should be safe, got %v", err)
		}
	}
}

// No goroutines remain after Close returns.
func TestDialerCloseNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	for range 10 {
		d, err := NewDialer(testDialerConfig())
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = d.Dial(context.Background()) }()
		time.Sleep(20 * time.Millisecond)
		_ = d.Close()
	}

	for range 100 {
		if runtime.NumGoroutine() <= before+3 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine leak: before=%d after=%d", before, runtime.NumGoroutine())
}

// Canceling the caller's own ctx must still work (mergeCancel must not swallow it).
func TestDialerRespectsCallerContext(t *testing.T) {
	d, err := NewDialer(testDialerConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err = d.Dial(ctx); err == nil {
		t.Fatal("Dial should fail after ctx times out")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("caller ctx ignored, took %v", elapsed)
	}
}

// mergeCancel propagates cancellation of either parent and leaks no goroutine after cancel.
func TestMergeCancel(t *testing.T) {
	t.Run("first parent canceled", func(t *testing.T) {
		a, cancelA := context.WithCancel(context.Background())
		b, cancelB := context.WithCancel(context.Background())
		defer cancelB()
		ctx, cancel := mergeCancel(a, b)
		defer cancel()
		cancelA()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Error("cancellation of a not propagated")
		}
	})

	t.Run("second parent canceled", func(t *testing.T) {
		a, cancelA := context.WithCancel(context.Background())
		defer cancelA()
		b, cancelB := context.WithCancel(context.Background())
		ctx, cancel := mergeCancel(a, b)
		defer cancel()
		cancelB()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Error("cancellation of b not propagated")
		}
	})

	t.Run("cancel does not leak", func(t *testing.T) {
		before := runtime.NumGoroutine()
		a, cancelA := context.WithCancel(context.Background())
		defer cancelA()
		b, cancelB := context.WithCancel(context.Background())
		defer cancelB()
		for range 50 {
			_, cancel := mergeCancel(a, b)
			cancel()
		}
		for range 50 {
			if runtime.NumGoroutine() <= before+2 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("mergeCancel leaked goroutines: before=%d after=%d", before, runtime.NumGoroutine())
	})
}
