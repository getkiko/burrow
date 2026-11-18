// Command echo-server shows that a burrow.Listener is an ordinary
// net.Listener by serving HTTP on it with the standard library. Burrow only
// establishes the connections; the protocol on top is up to the application.
//
// Usage:
//
//	go run ./examples/echo-server -signal signal.example.com:9960 -room demo
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/getkiko/burrow"
)

func main() {
	signal := flag.String("signal", "", "signal server address, e.g. signal.example.com:9960 (required)")
	room := flag.String("room", "demo", "room name; must match the client")
	iface := flag.String("iface", "", "network interface to bind, e.g. en0 or eth0 (default: kernel routing)")
	flag.Parse()
	if *signal == "" {
		log.Fatal("-signal is required")
	}

	ln, err := burrow.Listen(burrow.Config{
		SignalAddr:    *signal,
		Room:          *room,
		BindInterface: *iface,
	})
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	log.Printf("listening on %s, waiting for peers", ln.Addr())

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.Printf("%s %s from %s, body=%q", r.Method, r.URL.Path, r.RemoteAddr, body)
		fmt.Fprintf(w, "echo: %s\n", body)
		fmt.Fprintf(w, "server time: %s\n", time.Now().Format(time.RFC3339))
		fmt.Fprintf(w, "your address: %s\n", r.RemoteAddr)
	})

	// ln is a plain net.Listener, so the standard library can serve on it.
	if err := http.Serve(ln, mux); err != nil {
		log.Printf("server stopped: %v", err)
	}
}
