// Command burrow-signal is the signaling server used for hole punching. Run it
// on a host with a public IP address. It only introduces peers to each other
// and never relays application data.
//
// It serves two things over WebSocket:
//  1. Pairing: a session-addressed JSON protocol (see internal/signalproto).
//     A Listener registers a room, a Dialer starts a unique session, and
//     messages are delivered to the peer that owns the session. Because the
//     target is determined by the session, two Dialers can never be paired
//     with each other.
//  2. Address echo: a TCP flavour of STUN. The echo connection is made over
//     TCP because punching a TCP hole requires the real public TCP port.
//
// Every entry point is "<prefix>?command={signal|echo|nat|health}", where the
// prefix is set with -prefix. The fixed paths /signal, /echo, /probe, /nat and
// /health are also served.
//
// The server must be reachable directly over TCP: the address echo reports the
// connection's source address and echo tokens are bound to it, so it cannot
// run behind a reverse proxy.
//
// Usage: burrow-signal -listen :9960,:9962,:9963 [-prefix /signal] [-stat 127.0.0.1:9961]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

const sweepInterval = 10 * time.Second

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	listen := flag.String("listen", ":9960", "comma-separated listen addresses for WebSocket signaling and address echo")
	prefix := flag.String("prefix", "", "path prefix of the signaling entry point, e.g. /signal (empty means the root path)")
	stat := flag.String("stat", "", "listen address of the metrics endpoint, e.g. :9961 (empty disables it)")
	maxRoomsPerConn := flag.Int("max-rooms-per-conn", defaultMaxRoomsPerConn, "maximum rooms one signaling connection may listen on or dial into (0 disables the limit)")
	maxSignalPerIP := flag.Int("max-signal-per-ip", defaultMaxSignalPerIP, "maximum concurrent signaling connections per source IPv4 address or IPv6 /64 (0 disables the limit)")
	idleTimeout := flag.Duration("idle-timeout", defaultSignalIdleTimeout, "close a signaling connection that sends nothing for this long (0 disables it)")
	maxNATPerIP := flag.Int("max-nat-per-ip", defaultMaxNATPerIP, "maximum concurrent /nat mapping probes per source IPv4 address or IPv6 /64 (0 disables the limit)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("burrow-signal", version)
		return
	}
	listenAddresses := splitListenAddresses(*listen)
	if len(listenAddresses) == 0 {
		log.Fatal("at least one valid -listen address is required")
	}

	reg := newRegistry()
	reg.maxRoomsPerPeer = *maxRoomsPerConn
	service := newSignalService(reg, []byte(os.Getenv("BURROW_SIGNAL_SECRET")))
	service.natConns = newIPConnLimiter(*maxNATPerIP)
	service.signalConns = newIPConnLimiter(*maxSignalPerIP)
	service.idleTimeout = *idleTimeout

	var wg sync.WaitGroup
	for _, listenAddress := range listenAddresses {
		wg.Go(func() {
			if err := serveWebSocketSignal(listenAddress, *prefix, service); err != nil {
				log.Fatalf("[ws] %s exited: %v", listenAddress, err)
			}
		})
	}
	wg.Go(func() { sweepLoop(service, sweepInterval) })

	if *stat != "" {
		wg.Go(func() {
			log.Printf("[stat] metrics endpoint: http://%s/stat", *stat)
			serveStats(*stat, reg)
		})
	}

	log.Printf("burrow-signal %s started (protocol v%d), listening on %s", version, signalproto.Version, strings.Join(listenAddresses, ", "))
	wg.Wait()
}

func splitListenAddresses(value string) []string {
	seen := make(map[string]struct{})
	var addresses []string
	for _, address := range strings.Split(value, ",") {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if _, loaded := seen[address]; loaded {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	return addresses
}
