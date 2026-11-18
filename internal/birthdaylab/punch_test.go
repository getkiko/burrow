package birthdaylab

import (
	"context"
	"syscall"
	"testing"
	"time"
)

func TestLaunchDialSetWaitsForSocketControl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	specs := []dialSpec{
		{localAddr: "127.0.0.1:0", target: "127.0.0.1:1"},
		{localAddr: "127.0.0.1:0", target: "127.0.0.1:1"},
	}
	controlEntered := make(chan struct{}, len(specs))
	releaseControl := make(chan struct{})
	_, armed, done, counters := launchDialSetWithControl(
		ctx,
		specs,
		"arming-test",
		func(string, string, syscall.RawConn) error {
			controlEntered <- struct{}{}
			<-releaseControl
			return nil
		},
	)

	for range specs {
		select {
		case <-controlEntered:
		case <-ctx.Done():
			t.Fatal("socket control was not reached")
		}
	}
	select {
	case count := <-armed:
		t.Fatalf("arming completed before socket control returned: %d", count)
	default:
	}

	close(releaseControl)
	select {
	case count := <-armed:
		if count != len(specs) {
			t.Fatalf("armed = %d, want %d", count, len(specs))
		}
	case <-ctx.Done():
		t.Fatal("arming did not complete")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("dial workers did not finish")
	}
	if diagnostics := counters.snapshot(); diagnostics.Armed != len(specs) {
		t.Fatalf("diagnostics armed = %d, want %d", diagnostics.Armed, len(specs))
	}
}

func TestTrialParametersSignalRoundTrip(t *testing.T) {
	want, err := NewTrialParameters(386, 1024, 65535, 300*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := TrialParametersFromSignal(want.SignalParameters())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestTrialParametersRejectsExcessiveCandidates(t *testing.T) {
	if _, err := NewTrialParameters(MaxCandidates+1, 1024, 65535, time.Second, time.Second); err == nil {
		t.Fatal("excessive candidates must be rejected")
	}
}
