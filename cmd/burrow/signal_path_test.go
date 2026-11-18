package main

import "testing"

func TestApplySignalPath(t *testing.T) {
	tests := []struct {
		address    string
		signalPath string
		want       string
	}{
		{address: "signal.example:9960", want: "signal.example:9960"},
		{address: "signal.example:9960/embedded", want: "signal.example:9960/embedded"},
		{address: "signal.example:9960/embedded", signalPath: "signal/", want: "ws://signal.example:9960/signal"},
		{address: "wss://signal.example/embedded?old=1", signalPath: "/", want: "wss://signal.example/"},
	}
	for _, test := range tests {
		got, err := applySignalPath(test.address, test.signalPath)
		if err != nil {
			t.Fatalf("applySignalPath(%q, %q): %v", test.address, test.signalPath, err)
		}
		if got != test.want {
			t.Errorf("applySignalPath(%q, %q) = %q, want %q", test.address, test.signalPath, got, test.want)
		}
	}
}
