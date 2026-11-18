package burrow

import (
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
)

// The scan window is centered on basePort and keeps its width at the port
// range bounds.
func TestScanWindow(t *testing.T) {
	cases := []struct {
		base, rng          int
		wantStart, wantEnd int
	}{
		{base: 30000, rng: 512, wantStart: 29744, wantEnd: 30256},
		{base: 30000, rng: 0, wantStart: 30000, wantEnd: 30000},
		// At a bound the window shifts and still covers rng+1 ports.
		{base: 10, rng: 512, wantStart: 1, wantEnd: 513},
		{base: 65530, rng: 512, wantStart: 65023, wantEnd: 65535},
		{base: 1, rng: 2, wantStart: 1, wantEnd: 3},
	}
	for _, tc := range cases {
		start, end := scanWindow(tc.base, tc.rng)
		if start != tc.wantStart || end != tc.wantEnd {
			t.Errorf("scanWindow(%d,%d) = [%d,%d], want [%d,%d]",
				tc.base, tc.rng, start, end, tc.wantStart, tc.wantEnd)
		}
		if start < 1 || end > 65535 || start > end {
			t.Errorf("scanWindow(%d,%d) out of bounds: [%d,%d]", tc.base, tc.rng, start, end)
		}
	}
}

// The window always covers rng+1 ports: at the bounds it shifts instead of
// shrinking, otherwise coverage near low or high ports would silently drop.
func TestScanWindowWidthInvariant(t *testing.T) {
	for _, base := range []int{1, 2, 100, 1024, 30000, 60000, 65534, 65535} {
		for _, rng := range []int{1, 2, 64, 512, 4096} {
			start, end := scanWindow(base, rng)
			if width := end - start; width != rng {
				t.Errorf("scanWindow(%d,%d)=[%d,%d] width %d, want %d",
					base, rng, start, end, width, rng)
			}
		}
	}
}

// The window covers basePort itself: behind a cone NAT the reflected port is
// the real one.
func TestScanWindowCoversBase(t *testing.T) {
	for _, base := range []int{1, 2, 1024, 30000, 65535} {
		for _, rng := range []int{1, 2, 64, 512} {
			start, end := scanWindow(base, rng)
			if base < start || base > end {
				t.Errorf("scanWindow(%d,%d)=[%d,%d] does not cover basePort", base, rng, start, end)
			}
		}
	}
}

// Local errors (a misconfigured interface or source address) take priority
// over ordinary connection errors.
func TestRecordDialErrPrefersLocal(t *testing.T) {
	var slot atomic.Pointer[error]

	// Record some ordinary errors first.
	for range 5 {
		recordDialErr(&slot, syscall.ECONNREFUSED)
	}
	if got := *slot.Load(); !errors.Is(got, syscall.ECONNREFUSED) {
		t.Fatalf("first ordinary error should be recorded, got %v", got)
	}

	// A local error replaces ordinary errors.
	recordDialErr(&slot, syscall.EADDRNOTAVAIL)
	if got := *slot.Load(); !errors.Is(got, syscall.EADDRNOTAVAIL) {
		t.Fatalf("local error should replace ordinary error, got %v", got)
	}

	// Later ordinary errors must not replace the recorded local error.
	for range 100 {
		recordDialErr(&slot, syscall.ECONNREFUSED)
	}
	if got := *slot.Load(); !errors.Is(got, syscall.EADDRNOTAVAIL) {
		t.Errorf("local error replaced by ordinary error: %v", got)
	}
}

func TestRecordDialErrNilSafe(t *testing.T) {
	var slot atomic.Pointer[error]
	recordDialErr(&slot, nil)
	if slot.Load() != nil {
		t.Error("nil error should not be recorded")
	}
}

func TestIsLocalDialErr(t *testing.T) {
	local := []error{
		syscall.EADDRNOTAVAIL, syscall.EADDRINUSE, syscall.ENETUNREACH,
		syscall.EACCES, syscall.EMFILE, syscall.ENFILE,
	}
	for _, err := range local {
		if !isLocalDialErr(err) {
			t.Errorf("%v should be a local error", err)
		}
	}
	for _, err := range []error{syscall.ECONNREFUSED, syscall.ETIMEDOUT, errors.New("x")} {
		if isLocalDialErr(err) {
			t.Errorf("%v should not be a local error", err)
		}
	}
}
