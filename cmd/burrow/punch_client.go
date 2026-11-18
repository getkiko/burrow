package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/getkiko/burrow"
)

type punchClientConfig struct {
	signal     string
	signalPath string
	room       string
	message    string
	iface      string
	bench      bool
	size       string
}

func newPunchClientCommand() *cli.Command {
	cfg := &punchClientConfig{}
	return &cli.Command{
		Name:  "client",
		Usage: "punch a TCP path to the server and send one HTTP request over it",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "signal", Usage: "signal server address, as host:port/path", Required: true, Destination: &cfg.signal},
			newSignalPathFlag(&cfg.signalPath),
			&cli.StringFlag{Name: "room", Value: "demo", Usage: "room name; must match on both sides", Destination: &cfg.room},
			&cli.StringFlag{Name: "msg", Value: "ping from hole-punched client", Usage: "message to send", Destination: &cfg.message},
			&cli.StringFlag{Name: "iface", Usage: "physical interface to bind for punching and signaling, such as en0 or eth0; empty lets the kernel routing table choose", Destination: &cfg.iface},
			&cli.BoolFlag{Name: "bench", Usage: "transfer the given amount of data and report throughput", Destination: &cfg.bench},
			&cli.StringFlag{Name: "size", Value: "50MB", Usage: "bytes to transfer in each direction, e.g. 50MB or 1GB", Destination: &cfg.size},
		},
		Action: func(c *cli.Context) error {
			return runPunchClient(c.Context, c.App.Writer, *cfg)
		},
	}
}

func runPunchClient(ctx context.Context, output io.Writer, cfg punchClientConfig) error {
	logger := log.New(output, "", 0)
	if cfg.signal == "" {
		return fmt.Errorf("a signal server address is required")
	}
	signalAddress, err := applySignalPath(cfg.signal, cfg.signalPath)
	if err != nil {
		return err
	}
	cfg.signal = signalAddress

	var benchSize int64
	if cfg.bench {
		benchSize, err = parseSize(cfg.size)
		if err != nil {
			return fmt.Errorf("invalid size: %w", err)
		}
		logger.Printf("[bench] benchmark mode: %s per direction, about %s in total (upload + download)",
			formatBytes(benchSize), formatBytes(benchSize*2))
	}

	logger.Printf("[client] joining room %q via signal server %s, punching TCP ...", cfg.room, cfg.signal)
	dialer, err := burrow.NewDialer(burrow.Config{
		SignalAddr:    cfg.signal,
		Room:          cfg.room,
		BindInterface: cfg.iface,
		InfoLog:       logger.Printf,
	})
	if err != nil {
		return fmt.Errorf("[client] create dialer: %w", err)
	}
	defer dialer.Close()
	conn, err := dialer.Dial(ctx)
	if err != nil {
		return fmt.Errorf("[client] TCP hole punch failed: %w", err)
	}
	defer conn.Close()
	stopClosing := make(chan struct{})
	defer close(stopClosing)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClosing:
		}
	}()
	peer := conn.RemoteAddr().String()

	if cfg.bench {
		logger.Printf("[client] connected directly to %s, starting benchmark ...", peer)
		used := false
		transport := &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				if used {
					return nil, fmt.Errorf("the punched TCP connection can only be used once")
				}
				used = true
				return conn, nil
			},
			DisableCompression: true,
		}
		return benchHTTP(ctx, output, logger, &http.Client{Transport: transport}, "http://"+peer+benchPath, benchSize)
	}

	logger.Printf("[client] connected directly to %s, sending HTTP/1.1 request ...", peer)
	req := "POST / HTTP/1.1\r\n" +
		"Host: " + peer + "\r\n" +
		"Content-Type: text/plain\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(cfg.message)) +
		"Connection: close\r\n\r\n" + cfg.message
	start := time.Now()
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("[client] send request: %w", err)
	}
	response, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("[client] read response: %w", err)
	}
	logger.Printf("[client] done in %v, received %d response bytes",
		time.Since(start).Round(time.Millisecond), len(response))
	fmt.Fprintln(output, "---------- server response ----------")
	fmt.Fprint(output, string(response))
	fmt.Fprintln(output, "\n-------------------------------------")
	return nil
}

func benchHTTP(ctx context.Context, output io.Writer, logger *log.Logger, client *http.Client, url string, size int64) error {
	logger.Printf("[bench] starting benchmark, %s per direction -> %s", formatBytes(size), url)

	upProgress := newProgress(output, "upload", size)
	body := newPayload(size, upProgress.Add)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		upProgress.Stop()
		return fmt.Errorf("[bench] build request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Bench-Size", fmt.Sprintf("%d", size))

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		upProgress.Stop()
		return fmt.Errorf("[bench] request failed: %w", err)
	}
	defer resp.Body.Close()
	upBytes, upDuration := upProgress.Stop()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("[bench] server returned status %d", resp.StatusCode)
	}

	downProgress := newProgress(output, "download", size)
	buf := make([]byte, 64<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			downProgress.Add(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			downProgress.Stop()
			return fmt.Errorf("[bench] read response: %w", readErr)
		}
	}
	downBytes, downDuration := downProgress.Stop()
	total := time.Since(start)
	fmt.Fprintln(output, "\n============ benchmark result ============")
	fmt.Fprintf(output, "Upload:   %s in %v, %s\n",
		formatBytes(upBytes), upDuration.Round(time.Millisecond), formatRate(upBytes, upDuration))
	fmt.Fprintf(output, "Download: %s in %v, %s\n",
		formatBytes(downBytes), downDuration.Round(time.Millisecond), formatRate(downBytes, downDuration))
	fmt.Fprintf(output, "Total:    %s (upload + download)\n", formatBytes(upBytes+downBytes))
	fmt.Fprintf(output, "Elapsed:  %v\n", total.Round(time.Millisecond))
	fmt.Fprintln(output, "==========================================")
	return nil
}
