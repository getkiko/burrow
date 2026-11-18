package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// metrics holds global counters. Under heavy load there is no other way to
// tell which layer is the bottleneck, so even a small service exposes them.
var metrics struct {
	listen        atomic.Int64 // LISTEN messages received
	dial          atomic.Int64 // DIAL messages received
	ready         atomic.Int64 // READY messages received
	incoming      atomic.Int64 // INCOMING messages delivered to listeners
	peer          atomic.Int64 // PEER messages delivered to dialers
	peerResent    atomic.Int64 // PEER messages resent for a repeated DIAL (reflects PEER loss)
	noRoom        atomic.Int64 // no_listener replies (the listener is offline)
	badPacket     atomic.Int64 // messages that failed to parse
	dropped       atomic.Int64 // messages dropped because a queue was full (currently never incremented)
	rateLimited   atomic.Int64
	tokenRejected atomic.Int64
}

// serveStats serves a plain-text metrics endpoint, useful during load tests.
// It listens on its own address, separate from the signaling ports.
func serveStats(addr string, reg *registry) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stat", func(w http.ResponseWriter, _ *http.Request) {
		rooms, sessions := reg.Stats()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "rooms %d\n", rooms)
		fmt.Fprintf(w, "sessions_inflight %d\n", sessions)
		fmt.Fprintf(w, "listen_total %d\n", metrics.listen.Load())
		fmt.Fprintf(w, "dial_total %d\n", metrics.dial.Load())
		fmt.Fprintf(w, "ready_total %d\n", metrics.ready.Load())
		fmt.Fprintf(w, "incoming_total %d\n", metrics.incoming.Load())
		fmt.Fprintf(w, "peer_total %d\n", metrics.peer.Load())
		fmt.Fprintf(w, "peer_resent_total %d\n", metrics.peerResent.Load())
		fmt.Fprintf(w, "noroom_total %d\n", metrics.noRoom.Load())
		fmt.Fprintf(w, "bad_packet_total %d\n", metrics.badPacket.Load())
		fmt.Fprintf(w, "dropped_total %d\n", metrics.dropped.Load())
		fmt.Fprintf(w, "rate_limited_total %d\n", metrics.rateLimited.Load())
		fmt.Fprintf(w, "token_rejected_total %d\n", metrics.tokenRejected.Load())
	})
	_ = http.ListenAndServe(addr, mux)
}
