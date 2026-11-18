package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/getkiko/burrow/internal/birthdaylab"
	"github.com/getkiko/burrow/internal/signalproto"
)

func TestPrintTrialResultIncludesDiagnostics(t *testing.T) {
	var output bytes.Buffer
	result := birthdaylab.TrialResult{
		Elapsed: time.Second,
		Diagnostics: birthdaylab.AttemptDiagnostics{
			Requested:        256,
			Armed:            255,
			Connected:        1,
			ValidationErrors: 1,
			DialErrors: birthdaylab.DialErrorCounts{
				Timeout:  200,
				Canceled: 54,
				Resource: 1,
			},
		},
	}
	printTrialResult(&output, result, birthdaylab.Statistics{}.Add(result))
	for _, expected := range []string{
		"armed=255/256",
		"connected=1",
		"validation-errors=1",
		"timeout:200",
		"canceled:54",
		"resource:1",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("diagnostics missing %q:\n%s", expected, output.String())
		}
	}
}

func TestBirthdayServerUsesClientParameters(t *testing.T) {
	want, err := birthdaylab.NewTrialParameters(386, 2000, 60000, 300*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := birthdayServerTrialParameters(birthdaylab.IncomingRequest{
		Version:    signalproto.Version,
		Parameters: want.SignalParameters(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("parameters = %+v, want %+v", got, want)
	}
}

func TestBirthdayServerRejectsMissingParameters(t *testing.T) {
	if _, err := birthdayServerTrialParameters(birthdaylab.IncomingRequest{Version: signalproto.Version}); err == nil {
		t.Fatal("expected an error for a request without strategy parameters")
	}
}
