package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/getkiko/burrow/diag"
)

type tcpNATConfig struct {
	server     string
	signalPath string
	ports      string
	iface      string
	localPort  int
	rounds     int
	timeout    time.Duration
}

// newTCPNATCommand returns the tcpnat command. It connects from one local TCP
// port to several ports of the same public server and prints the public
// mapping the server observes, to classify how the TCP NAT allocates ports and
// whether the mapping depends on the destination port.
func newTCPNATCommand() *cli.Command {
	cfg := &tcpNATConfig{}
	return &cli.Command{
		Name:  "tcpnat",
		Usage: "probe TCP NAT mapping behavior across destination ports",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server", Usage: "public signal server address, IP:port/path", Required: true, Destination: &cfg.server},
			newSignalPathFlag(&cfg.signalPath),
			&cli.StringFlag{Name: "ports", Value: "9960,9962,9963", Usage: "comma-separated NAT probe ports", Destination: &cfg.ports},
			&cli.StringFlag{Name: "iface", Usage: "outgoing interface such as en0 or eth0; empty uses the kernel routing table", Destination: &cfg.iface},
			&cli.IntFlag{Name: "local-port", Usage: "fixed local TCP port; 0 picks a new port every round", Destination: &cfg.localPort},
			&cli.IntFlag{Name: "rounds", Value: 3, Usage: "number of probe rounds", Destination: &cfg.rounds},
			&cli.DurationFlag{Name: "timeout", Value: diag.DefaultProbeTimeout, Usage: "timeout for each connection", Destination: &cfg.timeout},
		},
		Action: func(c *cli.Context) error {
			return runTCPNAT(c.Context, c.App.Writer, *cfg)
		},
	}
}

// runTCPNAT runs diag.RunTCPNAT and prints its full text report.
func runTCPNAT(ctx context.Context, output io.Writer, cfg tcpNATConfig) error {
	result, err := diag.RunTCPNAT(ctx, diag.TCPNATConfig{
		Server:     cfg.server,
		Ports:      cfg.ports,
		Iface:      cfg.iface,
		LocalPort:  cfg.localPort,
		Rounds:     cfg.rounds,
		Timeout:    cfg.timeout,
		SignalPath: cfg.signalPath,
	})
	if result.Detail != "" {
		fmt.Fprint(output, result.Detail)
	}
	return err
}
