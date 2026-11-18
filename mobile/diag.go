package mobile

import (
	"context"
	"encoding/json"
	"time"

	"github.com/getkiko/burrow/diag"
)

// stunResultJSON is the STUN result sent to Kotlin.
type stunResultJSON struct {
	NATType     string `json:"NATType"`
	NATLabel    string `json:"NATLabel"`
	PublicAddr  string `json:"PublicAddr"`
	IsSymmetric bool   `json:"IsSymmetric"`
	Detail      string `json:"Detail"`
}

// RunSTUN runs STUN NAT type detection and returns the result as JSON.
//
// An empty server selects the default STUN server; an empty iface lets the
// kernel choose the route; timeoutMillis <= 0 uses the default timeout.
//
// Even when an error is returned, the Detail field of the JSON may contain
// partial output, which is worth showing to the user for troubleshooting.
func RunSTUN(server, iface string, timeoutMillis int) (out string, err error) {
	defer recoverToError(&err)

	cfg := diag.STUNConfig{Server: server, Iface: iface}
	if server == "" {
		cfg.Server = diag.DefaultSTUNServer
	}
	if timeoutMillis > 0 {
		cfg.Timeout = time.Duration(timeoutMillis) * time.Millisecond
	}

	result, runErr := diag.RunSTUN(context.Background(), cfg)
	return marshalDiagResult(stunResultJSON{
		NATType:     result.NATType,
		NATLabel:    result.NATLabel,
		PublicAddr:  result.PublicAddr,
		IsSymmetric: result.IsSymmetric,
		Detail:      result.Detail,
	}, runErr)
}

// tcpNATResultJSON is the TCP NAT probe result sent to Kotlin.
type tcpNATResultJSON struct {
	Detail string `json:"Detail"`
}

// RunTCPNAT probes TCP NAT mapping behavior and returns the result as JSON.
//
// ports is a comma-separated list of at least two distinct ports; localPort
// 0 picks a fresh local port each round; timeoutMillis <= 0 uses the
// default timeout.
func RunTCPNAT(server, ports, iface, proxy string, localPort, rounds, timeoutMillis int) (out string, err error) {
	defer recoverToError(&err)

	cfg := diag.TCPNATConfig{
		Server:    server,
		Ports:     ports,
		Iface:     iface,
		Proxy:     proxy,
		LocalPort: localPort,
		Rounds:    rounds,
	}
	if timeoutMillis > 0 {
		cfg.Timeout = time.Duration(timeoutMillis) * time.Millisecond
	}

	result, runErr := diag.RunTCPNAT(context.Background(), cfg)
	return marshalDiagResult(tcpNATResultJSON{Detail: result.Detail}, runErr)
}

// marshalDiagResult marshals a diagnostic result to JSON and combines it with
// the diagnostic's own error (runErr) into the (string, error) pair the
// exported functions return. A marshaling error takes precedence over runErr.
func marshalDiagResult[T any](result T, runErr error) (string, error) {
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return "", marshalErr
	}
	return string(data), runErr
}
