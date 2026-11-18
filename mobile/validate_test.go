package mobile

import (
	"strings"
	"testing"
)

// A valid config returns an empty string.
func TestValidateConfigAcceptsValid(t *testing.T) {
	if msg := ValidateConfig("client", validClientJSON); msg != "" {
		t.Errorf("valid config should return an empty string, got %q", msg)
	}
}

// An invalid config returns an error message.
func TestValidateConfigRejectsInvalid(t *testing.T) {
	msg := ValidateConfig("client", `{"listen":""}`)
	if msg == "" {
		t.Fatal("invalid config should return an error message")
	}
	if !strings.Contains(msg, "listen") {
		t.Errorf("error should mention the missing field, got %q", msg)
	}
}

// Malformed JSON returns an error message instead of panicking.
func TestValidateConfigMalformedJSON(t *testing.T) {
	if msg := ValidateConfig("client", `{not json`); msg == "" {
		t.Error("malformed JSON should return an error message")
	}
}

// The server role is validated as a ServerConfig.
func TestValidateConfigServerRole(t *testing.T) {
	const validServerJSON = `{
	  "forward": "127.0.0.1:29999",
	  "signal": "ws://127.0.0.1:1/p",
	  "room": "r",
	  "bind_interface": "lo0",
	  "strategy": "direct"
	}`
	if msg := ValidateConfig("server", validServerJSON); msg != "" {
		t.Errorf("valid server config should return an empty string, got %q", msg)
	}
	if msg := ValidateConfig("server", `{"forward":""}`); msg == "" {
		t.Error("invalid server config should return an error message")
	}
}

// An unknown role returns an explicit error.
func TestValidateConfigUnknownRole(t *testing.T) {
	msg := ValidateConfig("bogus", validClientJSON)
	if msg == "" {
		t.Fatal("unknown role should return an error message")
	}
	if !strings.Contains(msg, "bogus") && !strings.Contains(msg, "role") {
		t.Errorf("error should say the role is invalid, got %q", msg)
	}
}

// Validation must not start an instance.
func TestValidateConfigDoesNotStart(t *testing.T) {
	cleanup(t)
	ValidateConfig("client", validClientJSON)
	if !strings.Contains(Snapshot(), `"stopped"`) {
		t.Error("ValidateConfig should not start an instance")
	}
}

func TestVersion(t *testing.T) {
	if Version() == "" {
		t.Error("Version should not return an empty string")
	}
}
