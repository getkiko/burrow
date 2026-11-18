package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/getkiko/burrow/internal/birthdaylab"
	"github.com/getkiko/burrow/internal/signalproto"
	"github.com/urfave/cli/v2"
)

type birthdayClientConfig struct {
	signalAddress  string
	signalPath     string
	room           string
	iface          string
	candidates     int
	trials         int
	minPort        int
	maxPort        int
	warmup         time.Duration
	attemptTimeout time.Duration
	pairTimeout    time.Duration
}

func newBirthdayClientCommand() *cli.Command {
	cfg := &birthdayClientConfig{}
	return &cli.Command{
		Name:  "client",
		Usage: "run the APDM-side birthday client",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "signal", Required: true, Usage: "public signal server address, IP:port/path", Destination: &cfg.signalAddress},
			newSignalPathFlag(&cfg.signalPath),
			&cli.StringFlag{Name: "room", Value: "birthday-prototype", Usage: "experiment room", Destination: &cfg.room},
			&cli.StringFlag{Name: "iface", Usage: "physical interface attached to the mobile (carrier-grade NAT) network", Destination: &cfg.iface},
			&cli.IntFlag{Name: "candidates", Value: 256, Usage: "number of public mappings opened concurrently per trial", Destination: &cfg.candidates},
			&cli.IntFlag{Name: "trials", Value: 100, Usage: "number of trials", Destination: &cfg.trials},
			&cli.IntFlag{Name: "min-port", Value: 1024, Usage: "lower bound of the server random port range", Destination: &cfg.minPort},
			&cli.IntFlag{Name: "max-port", Value: 65535, Usage: "upper bound of the server random port range", Destination: &cfg.maxPort},
			&cli.DurationFlag{Name: "warmup", Value: 250 * time.Millisecond, Usage: "how long the stable side warms up SYN_SENT sockets before sending READY", Destination: &cfg.warmup},
			&cli.DurationFlag{Name: "attempt-timeout", Value: 3 * time.Second, Usage: "collision window per trial", Destination: &cfg.attemptTimeout},
			&cli.DurationFlag{Name: "pair-timeout", Value: 60 * time.Second, Usage: "signal pairing timeout per trial", Destination: &cfg.pairTimeout},
		},
		Action: func(c *cli.Context) error {
			return runBirthdayClient(c.Context, c.App.Writer, *cfg)
		},
	}
}

func runBirthdayClient(ctx context.Context, output io.Writer, cfg birthdayClientConfig) error {
	portCount, err := validateExperiment(cfg.candidates, cfg.trials, cfg.minPort, cfg.maxPort)
	if err != nil {
		return err
	}
	trialParameters, err := birthdaylab.NewTrialParameters(
		cfg.candidates,
		cfg.minPort,
		cfg.maxPort,
		cfg.warmup,
		cfg.attemptTimeout,
	)
	if err != nil {
		return err
	}
	signalAddress, err := applySignalPath(cfg.signalAddress, cfg.signalPath)
	if err != nil {
		return err
	}
	cfg.signalAddress = signalAddress
	bindIP, interfaceName, err := birthdaylab.ResolveBindIPv4(cfg.iface, cfg.signalAddress)
	if err != nil {
		return err
	}
	publicAddress, err := birthdaylab.EchoPublicAddress(ctx, cfg.signalAddress, bindIP)
	if err != nil {
		return fmt.Errorf("get public IP of the mobile network: %w", err)
	}
	theory := birthdaylab.CollisionProbability(cfg.candidates, cfg.candidates, portCount)
	fmt.Fprintln(output, "[client] PROTOTYPE: TCP birthday collision")
	fmt.Fprintf(output, "  signal=%s room=%s\n", cfg.signalAddress, cfg.room)
	fmt.Fprintf(output, "  interface=%s bind=%s observed-public=%s\n", interfaceName, bindIP, publicAddress)
	fmt.Fprintf(output, "  mappings=%d port-space=%d-%d theory=%.2f%%\n\n", cfg.candidates, cfg.minPort, cfg.maxPort, theory*100)

	var stats birthdaylab.Statistics
	for trial := 1; trial <= cfg.trials; trial++ {
		sessionID, err := birthdaylab.NewSessionID()
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "[client trial %03d/%03d] session=%s mappings=%d pairing\n",
			trial, cfg.trials, shortID(sessionID), cfg.candidates)
		serverAddress, err := pairWithRetry(
			ctx,
			cfg.pairTimeout,
			cfg.signalAddress,
			cfg.room,
			bindIP,
			sessionID,
			trialParameters.SignalParameters(),
		)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return fmt.Errorf("signal pairing: %w", err)
		}
		result := birthdaylab.RunClientAttempt(ctx, sessionID, bindIP, serverAddress, cfg.candidates, cfg.attemptTimeout)
		stats = stats.Add(result)
		printTrialResult(output, result, stats)
	}
	printTotal(output, "client", stats, theory)
	return nil
}

func pairWithRetry(
	parent context.Context,
	timeout time.Duration,
	signalAddress string,
	room string,
	bindIP string,
	sessionID string,
	parameters signalproto.StrategyParameters,
) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	for {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 10*time.Second)
		peer, err := birthdaylab.PairClient(attemptCtx, signalAddress, room, bindIP, sessionID, parameters)
		attemptCancel()
		if err == nil {
			return peer, nil
		}
		if !errors.Is(err, birthdaylab.ErrNoListener) {
			return "", err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}
