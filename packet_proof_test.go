package burrow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

func testPacketConfigForProof(t *testing.T, verify ProofVerifier) PacketConfig {
	t.Helper()
	cfg, err := PacketConfig{
		SignalAddr:  "127.0.0.1:1",
		Room:        "proof-room",
		STUN:        STUNOptions{Servers: []string{"127.0.0.1:1"}},
		Strategy:    StrategyDirect,
		VerifyProof: verify,
	}.normalize()
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	return cfg
}

// When VerifyProof fails, the listener must fail before allocating any
// candidate socket. The attempt would fail later anyway (SignalAddr is
// unreachable), so the test checks the point that matters: the proof
// failure stops the flow, which shows as zero socket budget in use.
func TestListenerRejectsBeforeSocketAllocationOnProofFailure(t *testing.T) {
	proofErr := errors.New("bad proof")
	cfg := testPacketConfigForProof(t, func(ctx ProofContext, proof string) error {
		return proofErr
	})

	limiter := newAttemptLimiter(cfg.MaxConcurrentAttempts)
	packetLimiter := newCandidatePacketLimiter()

	request := signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        cfg.Room,
		Session:     "sess-proof-1",
		Endpoint:    "203.0.113.10:4000",
		Strategy:    StrategyDirect,
		Fingerprint: cfg.strategyParameters().FingerprintForTransport(signalproto.TransportUDP),
		Transport:   signalproto.TransportUDP,
		Proof:       "", // missing or wrong proof
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := establishPacketListener(ctx, cfg, nil, request, limiter, packetLimiter)
	if !errors.Is(err, ErrProofRejected) {
		t.Fatalf("want ErrProofRejected, got %v", err)
	}

	// No candidate socket may be held: acquiring the full budget from the
	// same limiter must succeed at once (it would block and time out if
	// establishPacketListener had leaked a reservation).
	quickCtx, quickCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer quickCancel()
	release, acquireErr := limiter.acquireSockets(quickCtx, candidateSocketBudget)
	if acquireErr != nil {
		t.Fatalf("no candidate socket should be held after a proof failure, acquiring the full budget failed: %v", acquireErr)
	}
	release()
}

// When VerifyProof accepts, the flow must continue. It fails in a later
// network step, but not with ErrProofRejected.
func TestListenerContinuesWhenProofAccepted(t *testing.T) {
	cfg := testPacketConfigForProof(t, func(ctx ProofContext, proof string) error {
		return nil
	})
	limiter := newAttemptLimiter(cfg.MaxConcurrentAttempts)
	packetLimiter := newCandidatePacketLimiter()

	request := signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        cfg.Room,
		Session:     "sess-proof-2",
		Endpoint:    "203.0.113.10:4000",
		Strategy:    StrategyDirect,
		Fingerprint: cfg.strategyParameters().FingerprintForTransport(signalproto.TransportUDP),
		Transport:   signalproto.TransportUDP,
		Proof:       "anything",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := establishPacketListener(ctx, cfg, nil, request, limiter, packetLimiter)
	if errors.Is(err, ErrProofRejected) {
		t.Fatalf("accepted proof must not yield ErrProofRejected, got %v", err)
	}
}

// Without VerifyProof every message passes.
func TestListenerAcceptsWhenNoVerifyProofConfigured(t *testing.T) {
	cfg := testPacketConfigForProof(t, nil)
	limiter := newAttemptLimiter(cfg.MaxConcurrentAttempts)
	packetLimiter := newCandidatePacketLimiter()

	request := signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        cfg.Room,
		Session:     "sess-proof-3",
		Endpoint:    "203.0.113.10:4000",
		Strategy:    StrategyDirect,
		Fingerprint: cfg.strategyParameters().FingerprintForTransport(signalproto.TransportUDP),
		Transport:   signalproto.TransportUDP,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := establishPacketListener(ctx, cfg, nil, request, limiter, packetLimiter)
	if errors.Is(err, ErrProofRejected) {
		t.Fatalf("without VerifyProof, want no ErrProofRejected, got %v", err)
	}
}
