package diag

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	natstun "github.com/ccding/go-stun/stun"
)

func TestNormalizeSTUNServer(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "unchanged", input: "stun.miwifi.com:3478", want: "stun.miwifi.com:3478"},
		{name: "strip stun:// prefix", input: "stun://stun.miwifi.com:3478", want: "stun.miwifi.com:3478"},
		{name: "strip stuns:// prefix", input: "stuns://stun.miwifi.com:3478", want: "stun.miwifi.com:3478"},
		{name: "trim whitespace", input: "  stun.miwifi.com:3478  ", want: "stun.miwifi.com:3478"},
		{name: "empty is an error", input: "", wantErr: true},
		{name: "whitespace only is an error", input: "   ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeSTUNServer(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

// The stunNormalTypeFromString mapping feeds IsSymmetric, so pin every entry.
func TestSTUNNormalTypeMapping(t *testing.T) {
	cases := []struct {
		normalType string
		wantEN     string
		wantLabel  string
	}{
		{"Full cone NAT", "Full Cone NAT", "Full cone (NAT1)"},
		{"Restricted cone NAT", "Restricted Cone NAT", "Address-restricted cone (NAT2)"},
		{"Port Restricted cone NAT", "Port Restricted Cone NAT", "Port-restricted cone (NAT3)"},
		{"Symmetric NAT", "Symmetric NAT", "Symmetric (NAT4)"},
		{"Open Internet (no NAT)", "Open Internet", "No NAT"},
		{"unknown type", "Behavior Combination", "Mixed NAT behavior"},
	}
	for _, tc := range cases {
		t.Run(tc.normalType, func(t *testing.T) {
			en, label := stunNormalTypeFromString(tc.normalType)
			if en != tc.wantEN || label != tc.wantLabel {
				t.Errorf("= (%q, %q), want (%q, %q)", en, label, tc.wantEN, tc.wantLabel)
			}
		})
	}
}

// IsSymmetric is true only for a symmetric NAT; the server NAT self-check
// depends on it.
func TestIsSymmetricNATType(t *testing.T) {
	if !isSymmetricNATType("Symmetric NAT") {
		t.Error("Symmetric NAT should be classified as symmetric")
	}
	for _, other := range []string{
		"Full cone NAT", "Restricted cone NAT", "Port Restricted cone NAT",
		"Open Internet (no NAT)", "unknown type",
	} {
		if isSymmetricNATType(other) {
			t.Errorf("%q should not be classified as symmetric", other)
		}
	}
}

func TestSTUNBehaviorPresentation(t *testing.T) {
	behavior := natstun.NATBehavior{
		MappingType:   natstun.BehaviorTypeEndpoint,
		FilteringType: natstun.BehaviorTypeAddrAndPort,
	}
	var sb strings.Builder
	printSTUNBehavior(&sb, behavior)
	out := sb.String()
	if !strings.Contains(out, "[NAT type]") {
		t.Errorf("output is missing the NAT type line: %q", out)
	}
}

func TestAwaitSTUNOperationReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := awaitSTUNOperation(ctx, func() (int, error) {
		time.Sleep(2 * time.Second)
		return 1, nil
	})
	if err == nil {
		t.Fatal("expected an error when ctx is cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// An empty server must be rejected before any network I/O.
func TestRunSTUNRejectsEmptyServer(t *testing.T) {
	_, err := RunSTUN(context.Background(), STUNConfig{Server: ""})
	if err == nil {
		t.Fatal("expected an error for an empty server")
	}
}
