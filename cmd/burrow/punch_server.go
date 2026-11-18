package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/getkiko/burrow"
)

type punchServerConfig struct {
	signal     string
	signalPath string
	room       string
	iface      string
	bench      bool
}

func newPunchServerCommand() *cli.Command {
	cfg := &punchServerConfig{}
	return &cli.Command{
		Name:  "server",
		Usage: "accept a punched TCP connection and answer one HTTP request on it",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "signal", Usage: "signal server address, as host:port/path", Required: true, Destination: &cfg.signal},
			newSignalPathFlag(&cfg.signalPath),
			&cli.StringFlag{Name: "room", Value: "demo", Usage: "room name; must match on both sides", Destination: &cfg.room},
			&cli.StringFlag{Name: "iface", Usage: "physical interface to bind for punching and signaling, such as en0 or eth0; empty lets the kernel routing table choose", Destination: &cfg.iface},
			&cli.BoolFlag{Name: "bench", Usage: "serve benchmark requests from the client and report throughput", Destination: &cfg.bench},
		},
		Action: func(c *cli.Context) error {
			return runPunchServer(c.Context, c.App.Writer, *cfg)
		},
	}
}

func runPunchServer(ctx context.Context, output io.Writer, cfg punchServerConfig) error {
	logger := log.New(output, "", 0)
	if cfg.signal == "" {
		return fmt.Errorf("a signal server address is required")
	}
	signalAddress, err := applySignalPath(cfg.signal, cfg.signalPath)
	if err != nil {
		return err
	}
	cfg.signal = signalAddress
	if cfg.bench {
		logger.Printf("[bench] benchmark mode: waiting for the client (this uses real bandwidth)")
	}

	logger.Printf("[server] joining room %q via signal server %s ...", cfg.room, cfg.signal)
	// Punching parameters such as the port scan range are decided by the
	// dialer and sent over the signal channel, so the listener sets none.
	listener, err := burrow.Listen(burrow.Config{
		SignalAddr:    cfg.signal,
		Room:          cfg.room,
		BindInterface: cfg.iface,
		InfoLog:       logger.Printf,
	})
	if err != nil {
		return fmt.Errorf("[server] create listener: %w", err)
	}
	defer listener.Close()
	stopClosing := make(chan struct{})
	defer close(stopClosing)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopClosing:
		}
	}()

	if cfg.bench {
		logger.Printf("[server] listening, waiting for the benchmark connection ...")
		mux := http.NewServeMux()
		mux.HandleFunc(benchPath, newBenchHandler(logger))
		server := &http.Server{Handler: mux}
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
			return fmt.Errorf("[server] benchmark server stopped: %w", err)
		}
		logger.Printf("[server] benchmark finished")
		return nil
	}

	logger.Printf("[server] listening, waiting for an HTTP/1.1 request ...")
	conn, err := listener.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("[server] accept punched connection: %w", err)
	}
	defer conn.Close()
	peer := conn.RemoteAddr().String()
	logger.Printf("[server] TCP hole punch succeeded, connected directly to %s", peer)

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	request, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return fmt.Errorf("[server] read HTTP request: %w", err)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		logger.Printf("[server] read request body: %v", err)
	}
	_ = request.Body.Close()
	logger.Printf("[server] received %s %s, body=%q", request.Method, request.URL.Path, string(body))

	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	responseBody := fmt.Sprintf("hello from the hole-punched server\nyou sent: %s\nserver time: %s\nyour address: %s\n",
		string(body), time.Now().Format(time.RFC3339), peer)
	response := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(responseBody)) +
		"Connection: close\r\n\r\n" + responseBody
	if _, err := conn.Write([]byte(response)); err != nil {
		return fmt.Errorf("[server] write response: %w", err)
	}
	logger.Printf("[server] response sent, done")
	return nil
}

func newBenchHandler(logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		receiveStart := time.Now()
		n, err := io.Copy(&countingSink{}, r.Body)
		receiveDuration := time.Since(receiveStart)
		_ = r.Body.Close()
		if err != nil {
			logger.Printf("[bench] receive failed after %s: %v", formatBytes(n), err)
			http.Error(w, "recv error", http.StatusInternalServerError)
			return
		}
		logger.Printf("[bench] received %s upload in %v, %s",
			formatBytes(n), receiveDuration.Round(time.Millisecond), formatRate(n, receiveDuration))

		responseSize := n
		if value := r.Header.Get("X-Bench-Size"); value != "" {
			if parsed, parseErr := strconv.ParseInt(value, 10, 64); parseErr == nil && parsed > 0 {
				responseSize = parsed
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(responseSize, 10))
		w.WriteHeader(http.StatusOK)

		sendStart := time.Now()
		sent, err := io.Copy(w, newPayload(responseSize, nil))
		sendDuration := time.Since(sendStart)
		if err != nil {
			logger.Printf("[bench] send failed after %s: %v", formatBytes(sent), err)
			return
		}
		logger.Printf("[bench] sent %s download in %v, %s",
			formatBytes(sent), sendDuration.Round(time.Millisecond), formatRate(sent, sendDuration))
	}
}
