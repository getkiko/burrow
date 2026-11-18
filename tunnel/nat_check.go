package tunnel

import (
	"context"
	"errors"
	"fmt"

	"github.com/getkiko/burrow/diag"
)

// natCheckTimeout bounds the NAT self-check run before the server starts.
//
// It must be generous: full NAT type detection is not a single STUN round
// trip but several probes (mapping and filtering behavior each send to
// different IP/port combinations and wait for replies), and each may retry
// after packet loss. A short timeout (8s was tried) makes the probe time out
// every time, so the check never actually runs.
//
// It equals diag.DefaultSTUNTimeout (30s), which was sized for a complete
// detection; the self-check has no reason to be shorter.
const natCheckTimeout = diag.DefaultSTUNTimeout

// errSymmetricNAT is the sentinel error, matchable with errors.Is, returned
// when this host is behind a symmetric NAT.
var errSymmetricNAT = errors.New("tunnel: this host is behind a symmetric NAT and cannot act as the server")

// natProbeFunc abstracts NAT probing so tests can inject results without a
// real network.
//
// The probe uses UDP STUN (diag.RunSTUN) while the tunnel punches TCP holes
// (birthday_dial_set.go in package burrow uses tcp4). A NAT may map UDP and
// TCP differently, so "symmetric over UDP" only approximates TCP punching
// success; it is not proof. Only an explicit symmetric result causes a
// rejection, but the approximation itself cannot be removed here; exact
// TCP-level detection would need a different probing technique.
type natProbeFunc func(ctx context.Context, iface string) (diag.STUNResult, error)

// natProbe is the probe in use; tests may replace it.
var natProbe natProbeFunc = defaultNATProbe

// defaultNATProbe is the production probe, running a real STUN detection.
//
// It sets only Iface, not Timeout: the timeout is controlled solely by
// checkServerNAT through context.WithTimeout, so natCheckTimeout is the one
// place to change it. RunSTUN falls back to diag.DefaultSTUNTimeout (30s)
// when Timeout <= 0, which only matters for bare calls; the outer ctx bounds
// this one.
func defaultNATProbe(ctx context.Context, iface string) (diag.STUNResult, error) {
	return diag.RunSTUN(ctx, diag.STUNConfig{
		Server: diag.DefaultSTUNServer,
		Iface:  iface,
	})
}

// maybeCheckServerNAT runs the NAT self-check when it applies.
//
// It checks only when strategy == "birthday", matching the condition under
// which ServerConfig.Validate and burrow.Listen require StableSide; the check
// must not be stricter than what the SDK enforces. direct and predictive do
// not need a stable listener-side mapping and work behind a symmetric NAT,
// so they must not be refused by a birthday-specific error.
func maybeCheckServerNAT(ctx context.Context, skip bool, strategy, iface string, probe natProbeFunc, logger *Logger) error {
	if skip {
		logger.Infof("server NAT self-check skipped by config")
		return nil
	}
	if strategy != "birthday" {
		logger.Debugf("strategy=%s does not need a stable port mapping, skipping NAT self-check", strategy)
		return nil
	}
	return checkServerNAT(ctx, iface, probe, logger)
}

// checkServerNAT detects this host's NAT type and returns an error, refusing
// to start, for a symmetric NAT (NAT4).
//
// It is only called for strategy=birthday (gated by maybeCheckServerNAT); the
// error text is about birthday's stable-mapping requirement and means nothing
// for direct or predictive.
//
// A failed probe also refuses to start: the point of the check is to confirm
// before startup that the NAT is not symmetric, and passing on failure would
// make it meaningless. Skipping requires an explicit skip_nat_check: true, an
// informed choice by the user rather than a silent one by the code.
func checkServerNAT(ctx context.Context, iface string, probe natProbeFunc, logger *Logger) error {
	logger.Infof("detecting local NAT type (up to %s) ...", natCheckTimeout)

	checkCtx, cancel := context.WithTimeout(ctx, natCheckTimeout)
	defer cancel()

	result, err := probe(checkCtx, iface)
	if err != nil {
		return fmt.Errorf("NAT self-check did not complete: %w\n\n"+
			"The birthday strategy needs a stable port mapping on the server, so\n"+
			"startup must first confirm this host is not behind a symmetric NAT.\n"+
			"Starting without a result could leave punching failing for no visible reason.\n\n"+
			"Check that bind_interface is correct and that the STUN server is\n"+
			"reachable (default %s); you can also run the STUN diagnostic on its own.\n"+
			"To skip the check and start anyway, set skip_nat_check: true in the config.",
			err, diag.DefaultSTUNServer)
	}

	if !result.IsSymmetric {
		if result.NATType != "" {
			logger.Infof("NAT self-check passed: %s", result.NATType)
		} else {
			logger.Infof("NAT self-check passed")
		}
		return nil
	}

	return fmt.Errorf("%w\n\n"+
		"This host is behind a symmetric NAT (NAT4) and cannot act as the server.\n"+
		"The birthday strategy needs a stable port mapping on the server side; a\n"+
		"symmetric NAT allocates a new port for every new destination, and with\n"+
		"both sides unstable hole punching almost never succeeds.\n"+
		"Use a different network (wired, another carrier) or run the server on the other peer.\n"+
		"To start anyway, set skip_nat_check: true in the config.", errSymmetricNAT)
}
