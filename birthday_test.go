package burrow

import (
	"context"
	"syscall"
	"testing"
	"time"
)

func TestLaunchBirthdayDialSetWaitsForSocketControl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	specs := []BirthdayDialSpec{
		{LocalAddr: "127.0.0.1:0", Target: "127.0.0.1:1"},
		{LocalAddr: "127.0.0.1:0", Target: "127.0.0.1:1"},
	}
	controlEntered := make(chan struct{}, len(specs))
	releaseControl := make(chan struct{})
	dialSet := LaunchBirthdayDialSet(ctx, specs, func(string, string, syscall.RawConn) error {
		controlEntered <- struct{}{}
		<-releaseControl
		return nil
	})

	for range specs {
		select {
		case <-controlEntered:
		case <-ctx.Done():
			t.Fatal("socket control was not reached")
		}
	}
	select {
	case count := <-dialSet.Armed:
		t.Fatalf("arming completed before socket control returned: %d", count)
	default:
	}

	close(releaseControl)
	select {
	case count := <-dialSet.Armed:
		if count != len(specs) {
			t.Fatalf("armed = %d, want %d", count, len(specs))
		}
	case <-ctx.Done():
		t.Fatal("arming did not complete")
	}
	select {
	case <-dialSet.Done:
	case <-ctx.Done():
		t.Fatal("dial workers did not finish")
	}
	diagnostics := dialSet.Diagnostics()
	if diagnostics.Requested != len(specs) || diagnostics.Armed != len(specs) {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}
