package mobile

import (
	"encoding/json"
	"strings"
	"testing"
)

// Arguments are validated before any network operation.
func TestRunTCPNATRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		server string
		ports  string
		rounds int
	}{
		{name: "empty server", server: "", ports: "9960,9962", rounds: 3},
		{name: "zero rounds", server: "1.2.3.4:9960", ports: "9960,9962", rounds: 0},
		{name: "invalid ports", server: "1.2.3.4:9960", ports: "abc", rounds: 3},
		{name: "single port", server: "1.2.3.4:9960", ports: "9960", rounds: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RunTCPNAT(tc.server, tc.ports, "", "", 0, tc.rounds, 1000); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// The failure path also returns valid JSON (possibly with partial output).
func TestRunSTUNReturnsValidJSONOnFailure(t *testing.T) {
	out, err := RunSTUN("127.0.0.1:1", "", 500)
	if err == nil {
		t.Skip("unexpectedly reachable, skipping")
	}
	if out != "" {
		var parsed map[string]any
		if jsonErr := json.Unmarshal([]byte(out), &parsed); jsonErr != nil {
			t.Errorf("failure path returned invalid JSON: %v (raw %q)", jsonErr, out)
		}
	}
}

// Extreme arguments must not crash the process: checks the panic recovery.
func TestDiagFunctionsDoNotPanic(t *testing.T) {
	RunSTUN(strings.Repeat("x", 10000), "", -1)
	RunTCPNAT(strings.Repeat("x", 10000), "", "", "", -1, -1, -1)
}
