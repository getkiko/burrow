package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/urfave/cli/v2"
)

const defaultAPIListenAddress = "127.0.0.1:18083"

type apiConfig struct {
	listen string
}

func newAPICommand() *cli.Command {
	cfg := &apiConfig{}
	return &cli.Command{
		Name:  "api",
		Usage: "serve the HTTP API and embedded web console",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "listen", Value: defaultAPIListenAddress, Usage: "HTTP listen address; must be a loopback address or localhost", Destination: &cfg.listen},
		},
		Action: func(c *cli.Context) error {
			return runAPIServer(c.Context, *cfg)
		},
	}
}

// checkAPIListenAddress refuses non-loopback listen addresses. The console
// has no authentication: its Host check only stops browsers from being
// turned against it, and any other machine that can reach the port could
// simply send "Host: localhost". Loopback is therefore the only safe bind.
func checkAPIListenAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("api: invalid listen address %q: %w", address, err)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("api: listen address %q is not a loopback address; the console has no authentication, so bind it to 127.0.0.1, [::1] or localhost and use an SSH tunnel for remote access", address)
	}
	return nil
}

func runAPIServer(ctx context.Context, cfg apiConfig) error {
	if err := checkAPIListenAddress(cfg.listen); err != nil {
		return err
	}
	handler, err := newAPIHTTPServer()
	if err != nil {
		return err
	}
	defer handler.runs.stopAll()

	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return fmt.Errorf("api listen on %s: %w", cfg.listen, err)
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	fmt.Printf("[api] listening on http://%s\n", listener.Addr())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}
