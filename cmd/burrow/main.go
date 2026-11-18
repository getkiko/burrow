package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v2"
)

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newApp().RunContext(ctx, os.Args); err != nil {
		log.Fatal(err)
	}
}

// version is injected at build time with -ldflags "-X main.version=...".
var version = "dev"

func newApp() *cli.App {
	return &cli.App{
		Name:    "burrow",
		Usage:   "TCP hole punching between hosts behind NAT",
		Version: version,
		Commands: []*cli.Command{
			newTunnelCommand(),
			newAPICommand(),
			newPunchCommand(),
			newTCPNATCommand(),
			newSTUNCommand(),
			newBirthdayCommand(),
		},
	}
}
