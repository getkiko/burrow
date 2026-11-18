package main

import (
	"fmt"
	"log"

	"github.com/urfave/cli/v2"

	"github.com/getkiko/burrow/tunnel"
)

// tunnelRunner is the lifecycle shared by the tunnel client and server.
type tunnelRunner interface {
	Start() error
	Close() error
}

func newTunnelCommand() *cli.Command {
	return &cli.Command{
		Name:  "tunnel",
		Usage: "run a TCP port-forwarding tunnel over punched connections",
		Description: "The client listens on a local TCP address and carries every accepted\n" +
			"connection over its own punched TCP connection; the server forwards each\n" +
			"one to a fixed TCP address. Connections carry raw bytes with no extra headers.\n" +
			"See tunnel/example/ for sample configuration files.",
		Subcommands: []*cli.Command{
			newTunnelRoleCommand("client", "listen locally and forward connections through the tunnel"),
			newTunnelRoleCommand("server", "accept tunnel connections and forward them to a fixed address"),
		},
	}
}

func newTunnelRoleCommand(role, usage string) *cli.Command {
	var configPath string
	return &cli.Command{
		Name:  role,
		Usage: usage,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "c", Aliases: []string{"config"}, Usage: "path to the JSON configuration file", Required: true, Destination: &configPath},
		},
		Action: func(c *cli.Context) error {
			log.SetFlags(log.LstdFlags)
			instance, err := newTunnelRunner(role, configPath)
			if err != nil {
				return fmt.Errorf("start %s: %w", role, err)
			}
			if err := instance.Start(); err != nil {
				return fmt.Errorf("start %s: %w", role, err)
			}
			<-c.Context.Done()
			log.Println("shutting down ...")
			_ = instance.Close()
			return nil
		},
	}
}

// newTunnelRunner loads the configuration for role and builds the instance.
// Client and server configurations differ (the client has listeners, the
// server has a forward target, and negotiated parameters may only be set on
// the client), so each role uses its own loader and validation.
func newTunnelRunner(role, configPath string) (tunnelRunner, error) {
	if role == "client" {
		cfg, err := tunnel.LoadClientConfig(configPath)
		if err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
		return tunnel.NewClient(cfg, tunnel.LogObserver{})
	}
	cfg, err := tunnel.LoadServerConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return tunnel.NewServer(cfg, tunnel.LogObserver{})
}
