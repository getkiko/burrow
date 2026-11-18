package diag

import (
	"context"
	"reflect"
	"testing"
)

// ParsePorts requires at least two distinct ports.
func TestParsePorts(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []int
		wantErr bool
	}{
		{name: "multiple ports", input: "9960,9962,9963", want: []int{9960, 9962, 9963}},
		{name: "with spaces", input: "9960, 9962 , 9963", want: []int{9960, 9962, 9963}},
		{name: "deduplicate", input: "9960,9962,9960", want: []int{9960, 9962}},
		{name: "single port is an error", input: "9960", wantErr: true},
		{name: "empty is an error", input: "", wantErr: true},
		{name: "non-numeric is an error", input: "9960,abc", wantErr: true},
		{name: "out of range is an error", input: "70000", wantErr: true},
		{name: "zero port is an error", input: "0,9960", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePorts(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSplitServerPrefix(t *testing.T) {
	cases := []struct {
		in         string
		wantHost   string
		wantPrefix string
	}{
		{"10.xxx.xxx.xxx/signal", "10.xxx.xxx.xxx", "/signal"},
		{"10.xxx.xxx.xxx", "10.xxx.xxx.xxx", "/"},
		{"10.xxx.xxx.xxx/", "10.xxx.xxx.xxx", "/"},
		{"ws://10.xxx.xxx.xxx/signal", "10.xxx.xxx.xxx", "/signal"},
		{"http://10.xxx.xxx.xxx/signal/", "10.xxx.xxx.xxx", "/signal"},
		{"  example.com/a/b  ", "example.com", "/a/b"},
	}
	for _, item := range cases {
		t.Run(item.in, func(t *testing.T) {
			host, prefix := splitServerPrefix(item.in)
			if host != item.wantHost || prefix != item.wantPrefix {
				t.Errorf("splitServerPrefix(%q) = (%q, %q), want (%q, %q)",
					item.in, host, prefix, item.wantHost, item.wantPrefix)
			}
		})
	}
}

func TestResolveTCPNATServer(t *testing.T) {
	cases := []struct {
		name       string
		server     string
		signalPath string
		wantHost   string
		wantPath   string
	}{
		{name: "server with embedded prefix", server: "signal.example/signal", wantHost: "signal.example", wantPath: "/signal"},
		{name: "signalPath overrides prefix", server: "signal.example/legacy", signalPath: "api/signal/", wantHost: "signal.example", wantPath: "/api/signal"},
		{name: "signalPath is root", server: "signal.example/legacy", signalPath: "/", wantHost: "signal.example", wantPath: "/"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			host, path := resolveTCPNATServer(item.server, item.signalPath)
			if host != item.wantHost || path != item.wantPath {
				t.Errorf("resolveTCPNATServer(%q, %q) = (%q, %q), want (%q, %q)",
					item.server, item.signalPath, host, path, item.wantHost, item.wantPath)
			}
		})
	}
}

func TestMakeTargets(t *testing.T) {
	got := makeTargets("1.2.3.4", []int{9960, 9962})
	want := []string{"1.2.3.4:9960", "1.2.3.4:9962"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("= %v, want %v", got, want)
	}
}

// Input is validated before any network I/O.
func TestRunTCPNATValidatesInput(t *testing.T) {
	cases := []struct {
		name string
		cfg  TCPNATConfig
	}{
		{name: "empty server", cfg: TCPNATConfig{Server: "", Rounds: 3}},
		{name: "zero rounds", cfg: TCPNATConfig{Server: "1.2.3.4:9960", Rounds: 0}},
		{name: "negative rounds", cfg: TCPNATConfig{Server: "1.2.3.4:9960", Rounds: -1}},
		{name: "invalid ports", cfg: TCPNATConfig{Server: "1.2.3.4:9960", Rounds: 3, Ports: "abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RunTCPNAT(context.Background(), tc.cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
