package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/urfave/cli/v2"
)

func newSignalPathFlag(destination *string) *cli.StringFlag {
	return &cli.StringFlag{
		Name:        "signal_path",
		Aliases:     []string{"signal-path"},
		Usage:       "signal server path prefix such as /signal; when empty, the path in the signal address is used",
		Destination: destination,
	}
}

func applySignalPath(address, signalPath string) (string, error) {
	if strings.TrimSpace(signalPath) == "" {
		return address, nil
	}
	parsedAddress := address
	if !strings.Contains(parsedAddress, "://") {
		parsedAddress = "ws://" + parsedAddress
	}
	parsed, err := url.Parse(parsedAddress)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid signal address %q", address)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("signal address must use ws or wss: %q", address)
	}
	parsed.Path = normalizeSignalPath(signalPath)
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func normalizeSignalPath(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "/"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return value
}
