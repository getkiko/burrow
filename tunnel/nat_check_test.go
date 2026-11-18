package tunnel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/getkiko/burrow/diag"
)

// A symmetric NAT must be rejected, with an error that explains why and what
// to do.
func TestCheckServerNATRejectsSymmetric(t *testing.T) {
	probe := func(ctx context.Context, iface string) (diag.STUNResult, error) {
		return diag.STUNResult{
			NATType:     "Symmetric NAT",
			IsSymmetric: true,
		}, nil
	}
	err := checkServerNAT(context.Background(), "en0", probe, newLogger("error"))
	if err == nil {
		t.Fatal("a symmetric NAT should refuse to start")
	}
	if !errors.Is(err, errSymmetricNAT) {
		t.Errorf("error should match errSymmetricNAT via errors.Is, got %v", err)
	}
	for _, keyword := range []string{"symmetric", "server", "hole punching"} {
		if !strings.Contains(err.Error(), keyword) {
			t.Errorf("error is missing keyword %q: %v", keyword, err)
		}
	}
}

// Every non-symmetric NAT passes.
func TestCheckServerNATAllowsNonSymmetric(t *testing.T) {
	for _, natType := range []string{
		"Full Cone NAT", "Restricted Cone NAT",
		"Port Restricted Cone NAT", "Open Internet",
	} {
		t.Run(natType, func(t *testing.T) {
			probe := func(ctx context.Context, iface string) (diag.STUNResult, error) {
				return diag.STUNResult{NATType: natType, IsSymmetric: false}, nil
			}
			if err := checkServerNAT(context.Background(), "en0", probe, newLogger("error")); err != nil {
				t.Errorf("%s should pass, got error: %v", natType, err)
			}
		})
	}
}

// A failed probe refuses to start.
//
// Passing on probe failure would make the check meaningless: with a short
// timeout the full detection never finishes, so every start would silently
// pass while the user believes the check ran. Skipping requires an explicit
// skip_nat_check.
func TestCheckServerNATRejectsOnProbeFailure(t *testing.T) {
	probe := func(ctx context.Context, iface string) (diag.STUNResult, error) {
		return diag.STUNResult{}, errors.New("STUN server unreachable")
	}
	err := checkServerNAT(context.Background(), "en0", probe, newLogger("error"))
	if err == nil {
		t.Fatal("a failed probe should refuse to start, but it passed")
	}
	if !strings.Contains(err.Error(), "skip_nat_check") {
		t.Errorf("error should explain how to skip the check, got: %v", err)
	}
}

// A probe timeout also refuses to start.
func TestCheckServerNATRejectsOnTimeout(t *testing.T) {
	probe := func(ctx context.Context, iface string) (diag.STUNResult, error) {
		return diag.STUNResult{}, context.DeadlineExceeded
	}
	if err := checkServerNAT(context.Background(), "en0", probe, newLogger("error")); err == nil {
		t.Error("a probe timeout should refuse to start, but it passed")
	}
}

// When the config skips the check, the probe must not be called at all.
func TestServerConfigSkipNATCheck(t *testing.T) {
	called := false
	probe := func(ctx context.Context, iface string) (diag.STUNResult, error) {
		called = true
		return diag.STUNResult{IsSymmetric: true}, nil
	}
	// skip=true returns nil without calling probe.
	if err := maybeCheckServerNAT(context.Background(), true, "birthday", "en0", probe, newLogger("error")); err != nil {
		t.Errorf("skipped check should return nil, got %v", err)
	}
	if called {
		t.Error("skipped check should not call the probe")
	}
}

// direct and predictive do not need a stable listener-side mapping and work
// behind a symmetric NAT, so they must not be refused and the probe must not
// be called.
func TestMaybeCheckServerNATSkipsNonBirthdayStrategy(t *testing.T) {
	for _, strategy := range []string{"direct", "predictive"} {
		t.Run(strategy, func(t *testing.T) {
			called := false
			probe := func(ctx context.Context, iface string) (diag.STUNResult, error) {
				called = true
				return diag.STUNResult{IsSymmetric: true}, nil
			}
			if err := maybeCheckServerNAT(context.Background(), false, strategy, "en0", probe, newLogger("error")); err != nil {
				t.Errorf("strategy=%s should pass, got error: %v", strategy, err)
			}
			if called {
				t.Errorf("strategy=%s should not call the probe", strategy)
			}
		})
	}
}

// With birthday and no explicit skip, a symmetric result is rejected. This
// covers the full path from maybeCheckServerNAT to checkServerNAT.
func TestMaybeCheckServerNATRejectsSymmetricForBirthday(t *testing.T) {
	probe := func(ctx context.Context, iface string) (diag.STUNResult, error) {
		return diag.STUNResult{IsSymmetric: true, NATType: "Symmetric NAT"}, nil
	}
	err := maybeCheckServerNAT(context.Background(), false, "birthday", "en0", probe, newLogger("error"))
	if !errors.Is(err, errSymmetricNAT) {
		t.Errorf("strategy=birthday behind a symmetric NAT should be rejected, got %v", err)
	}
}
