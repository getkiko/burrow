package burrow

import (
	"testing"
)

// predictivePortSequence must expand center-out: P, P+1, P-1, P+2, P-2, ...
func TestPredictivePortSequenceCenterExpansion(t *testing.T) {
	got := predictivePortSequence(1000, 4)
	want := []int{1000, 1001, 999, 1002, 998}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d mismatch: got %v, want %v", i, got, want)
		}
	}
}

// scanRange is the window width excluding the center port, so the result
// has scanRange+1 entries when no boundary is hit.
func TestPredictivePortSequenceLength(t *testing.T) {
	for _, scanRange := range []int{0, 1, 4, 512, 4096} {
		got := predictivePortSequence(30000, scanRange)
		if len(got) != scanRange+1 {
			t.Errorf("scanRange=%d: length got %d, want %d", scanRange, len(got), scanRange+1)
		}
	}
}

// A scanRange above predictiveUDPMaxPortRange (4096) must be capped rather
// than expand a larger window.
func TestPredictivePortSequenceCapsAt4096(t *testing.T) {
	got := predictivePortSequence(30000, 100000)
	if len(got) != predictiveUDPMaxPortRange+1 {
		t.Fatalf("want %d entries after capping, got %d", predictiveUDPMaxPortRange+1, len(got))
	}
}

// Near the lower bound (1), out-of-range candidates (<= 0) are skipped
// rather than wrapped, and the expansion keeps going upward so the total
// still reaches scanRange+1 when the port space allows.
func TestPredictivePortSequenceNearLowerBound(t *testing.T) {
	got := predictivePortSequence(2, 5)
	for _, port := range got {
		if port < 1 || port > 65535 {
			t.Fatalf("out-of-range port %d (sequence %v)", port, got)
		}
	}
	// For basePort=2 the valid candidates are 2,3,1,4,5,6 (0 and -1 skipped).
	want := []int{2, 3, 1, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d mismatch: got %v, want %v", i, got, want)
		}
	}
}

// The same near the upper bound (65535).
func TestPredictivePortSequenceNearUpperBound(t *testing.T) {
	got := predictivePortSequence(65534, 5)
	for _, port := range got {
		if port < 1 || port > 65535 {
			t.Fatalf("out-of-range port %d (sequence %v)", port, got)
		}
	}
	want := []int{65534, 65535, 65533, 65532, 65531, 65530}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d mismatch: got %v, want %v", i, got, want)
		}
	}
}
