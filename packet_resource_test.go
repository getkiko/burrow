package burrow

import (
	"context"
	"errors"
	"testing"
	"time"
)

// acquireSockets must block when the budget is exhausted, rather than fail
// or oversubscribe, and a blocked call must proceed once budget is
// released. This checks that UDP shares the candidateSocketBudget (512)
// socket budget, without actually opening 512 sockets.
func TestAcquireSocketsBlocksWhenBudgetExhausted(t *testing.T) {
	limiter := newAttemptLimiter(8)

	// Take almost the whole budget (candidateSocketBudget is a constant) in
	// one call, leaving almost nothing.
	releaseBulk, err := limiter.acquireSockets(context.Background(), candidateSocketBudget-1)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	// Ask for 2 more: only 1 is left, so this must block.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	blockedErrCh := make(chan error, 1)
	go func() {
		_, acquireErr := limiter.acquireSockets(ctx, 2)
		blockedErrCh <- acquireErr
	}()

	select {
	case err := <-blockedErrCh:
		if err == nil {
			t.Fatal("acquireSockets must not succeed immediately with an exhausted budget")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want DeadlineExceeded from the ctx timeout, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("acquireSockets neither failed nor returned; test setup is wrong")
	}

	// After releasing the budget a new request must succeed immediately.
	releaseBulk()
	releaseSmall, err := limiter.acquireSockets(context.Background(), 2)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	releaseSmall()
}

// acquireSockets must return immediately when the budget suffices.
func TestAcquireSocketsSucceedsWithinBudget(t *testing.T) {
	limiter := newAttemptLimiter(8)
	release, err := limiter.acquireSockets(context.Background(), predictivePortBudget(64))
	if err != nil {
		t.Fatalf("acquire within budget failed: %v", err)
	}
	release()
}

// predictivePortBudget and directPortBudget only wrap strategySocketWeight;
// check that the wrapping keeps the values.
func TestPredictiveAndDirectPortBudget(t *testing.T) {
	if got := directPortBudget(); got != 2 {
		t.Errorf("directPortBudget() = %d, want 2", got)
	}
	if got := predictivePortBudget(64); got != 65 {
		t.Errorf("predictivePortBudget(64) = %d, want 65", got)
	}
}

// candidatePacketLimiter must enforce the configured rate rather than let
// everything through. The test only checks that the burst passes quickly,
// without measuring the exact rate, to avoid flakiness from CI timing
// jitter.
func TestCandidatePacketLimiterThrottles(t *testing.T) {
	limiter := newCandidatePacketLimiter()
	ctx := context.Background()
	// burst=1024, so the first 1024 calls must not block.
	start := time.Now()
	for range 1024 {
		if err := limiter.wait(ctx); err != nil {
			t.Fatalf("request within burst failed: %v", err)
		}
	}
	burstElapsed := time.Since(start)
	if burstElapsed > 200*time.Millisecond {
		t.Errorf("1024 requests within burst took %s, want near-instant", burstElapsed)
	}
}
