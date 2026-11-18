package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/getkiko/burrow/internal/birthdaylab"
	"github.com/urfave/cli/v2"
)

const defaultBirthdayProbePorts = "9960,9962,9963"

type birthdayServerConfig struct {
	signalAddress string
	signalPath    string
	probeSignals  string
	probePorts    string
	room          string
	iface         string
	mode          string
	localPort     int
	publicAddress string
	trials        int
}

func newBirthdayServerCommand() *cli.Command {
	cfg := &birthdayServerConfig{}
	return &cli.Command{
		Name:  "server",
		Usage: "run the stable-mapping-side birthday server",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "signal", Required: true, Usage: "public signal server address, IP:port/path", Destination: &cfg.signalAddress},
			newSignalPathFlag(&cfg.signalPath),
			&cli.StringFlag{
				Name:        "probe-signals",
				Aliases:     []string{"signal-servers"},
				Usage:       "several public signal servers (IP:port/path) separated by commas, spaces or newlines, used to verify the stable mapping",
				Destination: &cfg.probeSignals,
			},
			&cli.StringFlag{Name: "probe-ports", Value: defaultBirthdayProbePorts, Usage: "signal server ports used to verify the stable mapping", Hidden: true, Destination: &cfg.probePorts},
			&cli.StringFlag{Name: "room", Value: "birthday-prototype", Usage: "experiment room", Destination: &cfg.room},
			&cli.StringFlag{Name: "iface", Usage: "physical interface attached to the port-cone network", Destination: &cfg.iface},
			&cli.StringFlag{Name: "mode", Value: "auto", Usage: "server network type: auto, nat (home/office NAT) or fixed (public host or port-forwarded address)", Destination: &cfg.mode},
			&cli.IntFlag{Name: "local-port", Usage: "fixed local TCP port; used together with public-address", Destination: &cfg.localPort},
			&cli.StringFlag{Name: "public-address", Usage: "known stable public IP:port (cloud host baseline mode)", Destination: &cfg.publicAddress},
			&cli.IntFlag{Name: "trials", Value: 0, Usage: "maximum number of sessions to serve; 0 runs until interrupted", Hidden: true, Destination: &cfg.trials},
		},
		Action: func(c *cli.Context) error {
			return runBirthdayServer(c.Context, c.App.Writer, c.App.ErrWriter, *cfg)
		},
	}
}

func runBirthdayServer(ctx context.Context, output, errorOutput io.Writer, cfg birthdayServerConfig) error {
	signalAddress, err := applySignalPath(cfg.signalAddress, cfg.signalPath)
	if err != nil {
		return err
	}
	cfg.signalAddress = signalAddress
	bindIP, interfaceName, err := birthdaylab.ResolveBindIPv4(cfg.iface, cfg.signalAddress)
	if err != nil {
		return err
	}
	stableMode := strings.ToLower(strings.TrimSpace(cfg.mode))
	if stableMode == "auto" {
		stableMode = "nat"
		if cfg.publicAddress != "" {
			stableMode = "fixed"
		}
	}
	if stableMode != "nat" && stableMode != "fixed" {
		return fmt.Errorf("mode must be nat, fixed or auto")
	}

	var stable *birthdaylab.StableEndpoint
	if stableMode == "fixed" {
		if cfg.publicAddress == "" || cfg.localPort == 0 {
			return fmt.Errorf("mode fixed requires local-port and public-address")
		}
		stable, err = birthdaylab.FixedStableEndpoint(bindIP, cfg.localPort, cfg.publicAddress)
	} else {
		if strings.TrimSpace(cfg.probeSignals) != "" {
			probeSignals, parseErr := birthdaylab.ParseSignalAddresses(cfg.probeSignals)
			if parseErr != nil {
				return parseErr
			}
			stable, err = birthdaylab.OpenStableEndpointSignals(ctx, probeSignals, bindIP)
		} else {
			if strings.TrimSpace(cfg.probePorts) == "" {
				cfg.probePorts = defaultBirthdayProbePorts
			}
			probePorts, parseErr := birthdaylab.ParsePorts(cfg.probePorts)
			if parseErr != nil {
				return parseErr
			}
			stable, err = birthdaylab.OpenStableEndpoint(ctx, cfg.signalAddress, bindIP, probePorts)
		}
	}
	if err != nil {
		return fmt.Errorf("open stable server endpoint: %w", err)
	}
	defer stable.Close()
	sampler, err := birthdaylab.NewPortSampler()
	if err != nil {
		return err
	}
	control, err := birthdaylab.OpenListenerSignal(ctx, cfg.signalAddress, cfg.room, bindIP)
	if err != nil {
		return fmt.Errorf("register experiment server: %w", err)
	}
	defer control.Close()

	fmt.Fprintln(output, "[server] PROTOTYPE: TCP birthday collision")
	fmt.Fprintf(output, "  signal=%s room=%s\n", cfg.signalAddress, cfg.room)
	fmt.Fprintf(output, "  mode=%s interface=%s bind=%s stable-public=%s\n", stableMode, interfaceName, stable.LocalAddr, stable.PublicAddr)
	if stableMode == "nat" {
		fmt.Fprintln(output, "  signal-assisted mapping qualification:")
		for _, observation := range stable.Observations {
			fmt.Fprintf(output, "    %s -> %s => %s\n", stable.LocalAddr, observation.TargetAddr, observation.PublicAddr)
		}
		if stableObservationHostCount(stable.Observations) > 1 {
			fmt.Fprintln(output, "  qualification=same endpoint across multiple Signal IPs; EIM candidate across tested destinations")
		} else {
			fmt.Fprintln(output, "  qualification=same endpoint across Signal ports; EIM/ADM candidate")
		}
	}
	fmt.Fprintln(output, "  parameters=client-supplied per session")
	fmt.Fprintln(output)

	var stats birthdaylab.Statistics
	var theorySum float64
	seen := make(map[string]struct{})
	for cfg.trials <= 0 || stats.Trials < cfg.trials {
		incoming, err := control.WaitIncoming(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return fmt.Errorf("read client signal: %w", err)
		}
		sessionID := incoming.Session
		if _, exists := seen[sessionID]; exists {
			continue
		}
		seen[sessionID] = struct{}{}
		clientIP, err := birthdaylab.PublicIP(incoming.ClientAddress)
		if err != nil {
			fmt.Fprintf(errorOutput, "[server] session=%s invalid client address: %v\n", shortID(sessionID), err)
			_ = control.Reject(sessionID, "invalid_endpoint", err)
			continue
		}
		parameters, err := birthdayServerTrialParameters(incoming)
		if err != nil {
			fmt.Fprintf(errorOutput, "[server] session=%s invalid client parameters: %v\n", shortID(sessionID), err)
			_ = control.Reject(sessionID, "strategy_mismatch", err)
			continue
		}
		trialNumber := stats.Trials + 1
		fmt.Fprintf(output, "[server trial %03d] session=%s client-ip=%s arming=%d\n",
			trialNumber, shortID(sessionID), clientIP, parameters.Candidates)
		fmt.Fprintf(output, "  parameters ports=%d-%d warmup=%s timeout=%s\n",
			parameters.MinPort,
			parameters.MaxPort,
			parameters.Warmup,
			parameters.AttemptTimeout,
		)
		result, err := birthdaylab.RunServerAttempt(
			ctx,
			sampler,
			sessionID,
			stable.LocalAddr,
			clientIP,
			parameters.MinPort,
			parameters.MaxPort,
			parameters.Candidates,
			parameters.Warmup,
			parameters.AttemptTimeout,
			func() error { return control.Ready(sessionID, stable.PublicAddr) },
		)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(errorOutput, "  control-error=%v\n", err)
			_ = control.Reject(sessionID, "punch_failed", err)
		}
		stats = stats.Add(result)
		portCount := parameters.MaxPort - parameters.MinPort + 1
		theorySum += birthdaylab.CollisionProbability(parameters.Candidates, parameters.Candidates, portCount)
		printTrialResult(output, result, stats)
	}
	if stats.Trials > 0 {
		printTotal(output, "server", stats, theorySum/float64(stats.Trials))
	}
	return nil
}

// birthdayServerTrialParameters returns the per-session trial parameters the
// client sent in its signal request.
func birthdayServerTrialParameters(incoming birthdaylab.IncomingRequest) (birthdaylab.TrialParameters, error) {
	return birthdaylab.TrialParametersFromSignal(incoming.Parameters)
}

func stableObservationHostCount(observations []birthdaylab.MappingObservation) int {
	hosts := make(map[string]struct{}, len(observations))
	for _, observation := range observations {
		host, _, err := net.SplitHostPort(observation.TargetAddr)
		if err == nil {
			hosts[host] = struct{}{}
		}
	}
	return len(hosts)
}
