package mobile

import (
	"encoding/json"
	"fmt"

	"github.com/getkiko/burrow/tunnel"
)

// version is injected at build time via -ldflags; it defaults to "dev".
var version = "dev"

// Version returns the version of the binding layer.
func Version() string {
	return version
}

// ValidateConfig validates a config without starting anything; the config
// screen uses it as a pre-save check.
//
// It returns an empty string if the config is valid, otherwise an error
// message that can be shown to the user. role is "client" or "server".
//
// It reuses the tunnel package's validation, the same rules applied on a
// real start, so a config never passes here and then fails validation at
// start time.
func ValidateConfig(role string, configJSON string) (out string) {
	defer recoverToString(&out, "internal error while validating config")

	switch role {
	case "client":
		var cfg tunnel.ClientConfig
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return "parse config: " + err.Error()
		}
		if err := cfg.Validate(); err != nil {
			return err.Error()
		}
		return ""
	case "server":
		var cfg tunnel.ServerConfig
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return "parse config: " + err.Error()
		}
		if err := cfg.Validate(); err != nil {
			return err.Error()
		}
		return ""
	default:
		return fmt.Sprintf("unknown role %q, want client or server", role)
	}
}
