// Command echo-client shows that a burrow.Dialer returns ordinary net.Conns
// by plugging it into the standard library's http.Transport. Every new HTTP
// connection triggers one hole punching round.
//
// Usage:
//
//	go run ./examples/echo-client -signal signal.example.com:9960 -room demo -msg hello
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/getkiko/burrow"
)

func main() {
	signal := flag.String("signal", "", "signal server address, e.g. signal.example.com:9960 (required)")
	room := flag.String("room", "demo", "room name; must match the server")
	iface := flag.String("iface", "", "network interface to bind, e.g. en0 or eth0 (default: kernel routing)")
	msg := flag.String("msg", "hello from burrow", "message to send")
	flag.Parse()
	if *signal == "" {
		log.Fatal("-signal is required")
	}

	d, err := burrow.NewDialer(burrow.Config{
		SignalAddr:    *signal,
		Room:          *room,
		BindInterface: *iface,
	})
	if err != nil {
		log.Fatalf("create dialer: %v", err)
	}
	defer d.Close()

	transport := &http.Transport{
		// Ignore network and addr: the Dialer always connects to the peer
		// listening in the room.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.Dial(ctx)
		},
		// A new connection per request, so every request punches again.
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Minute}

	log.Print("punching to peer")
	start := time.Now()
	// The URL host only fills the Host header; the Dialer decides where the
	// connection goes.
	resp, err := client.Post("http://burrow.invalid/", "text/plain", strings.NewReader(*msg))
	if err != nil {
		log.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("read response: %v", err)
	}
	log.Printf("done in %v, status %d", time.Since(start).Round(time.Millisecond), resp.StatusCode)
	fmt.Print(string(body))
}
