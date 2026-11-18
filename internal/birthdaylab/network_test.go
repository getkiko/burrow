package birthdaylab

import (
	"reflect"
	"testing"
)

func TestWebSocketURLUsesConfiguredPath(t *testing.T) {
	tests := []struct {
		address      string
		fallbackPath string
		overridePort int
		want         string
	}{
		{
			address:      "signal.example:9960",
			fallbackPath: "/signal",
			want:         "ws://signal.example:9960/signal?command=signal&room=birthday-prototype&session=probe",
		},
		{
			address:      "ws://signal.example:9960/signal",
			fallbackPath: "/nat",
			overridePort: 9962,
			want:         "ws://signal.example:9962/signal?command=nat&room=birthday-prototype&session=probe",
		},
		{
			address:      "wss://signal.example/",
			fallbackPath: "/echo",
			want:         "wss://signal.example:443/?command=echo&room=birthday-prototype&session=probe",
		},
	}
	for _, test := range tests {
		got, err := websocketURL(test.address, test.fallbackPath, test.overridePort)
		if err != nil {
			t.Fatalf("websocketURL(%q, %q): %v", test.address, test.fallbackPath, err)
		}
		if got != test.want {
			t.Errorf("websocketURL(%q, %q) = %q, want %q", test.address, test.fallbackPath, got, test.want)
		}
	}
}

func TestParseSignalAddressesWithEmbeddedPaths(t *testing.T) {
	got, err := ParseSignalAddresses("203.0.113.10:9960/signal, 198.51.100.20:9962/edge\n203.0.113.10:9960/signal")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"203.0.113.10:9960/signal", "198.51.100.20:9962/edge"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %#v, want %#v", got, want)
	}
}

func TestParseSignalAddressesRejectsHostname(t *testing.T) {
	if _, err := ParseSignalAddresses("signal.example:9960/path"); err == nil {
		t.Fatal("hostname probe signal must be rejected")
	}
}

func TestSignalHostPortAcceptsEmbeddedPath(t *testing.T) {
	got, err := SignalHostPort("203.0.113.10:9960/signal")
	if err != nil {
		t.Fatal(err)
	}
	if got != "203.0.113.10:9960" {
		t.Fatalf("SignalHostPort = %q", got)
	}
}
