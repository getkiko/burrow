package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/getkiko/burrow/diag"
)

type stunConfig struct {
	server  string
	iface   string
	timeout time.Duration
}

func newSTUNCommand() *cli.Command {
	cfg := &stunConfig{}
	return &cli.Command{
		Name:  "stun",
		Usage: "detect UDP NAT mapping and filtering behavior with STUN",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "stun", Value: diag.DefaultSTUNServer, Usage: "STUN server address", Destination: &cfg.server},
			&cli.StringFlag{Name: "iface", Usage: "outgoing interface such as en0 or eth0; empty uses the kernel routing table", Destination: &cfg.iface},
			&cli.DurationFlag{Name: "timeout", Value: diag.DefaultSTUNTimeout, Usage: "timeout for the full NAT type test", Destination: &cfg.timeout},
		},
		Action: func(c *cli.Context) error {
			return runSTUN(c.Context, c.App.Writer, *cfg)
		},
	}
}

// runSTUN runs diag.RunSTUN and prints its full text report.
func runSTUN(ctx context.Context, output io.Writer, cfg stunConfig) error {
	if output == nil {
		output = io.Discard
	}
	if cfg.timeout <= 0 {
		return fmt.Errorf("timeout must be greater than 0")
	}
	result, err := diag.RunSTUN(ctx, diag.STUNConfig{
		Server:  cfg.server,
		Iface:   cfg.iface,
		Timeout: cfg.timeout,
	})
	if result.Detail != "" {
		fmt.Fprint(output, result.Detail)
	}
	return err
}
