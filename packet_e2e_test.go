package burrow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/signalproto"
)

// This file provides a minimal UDP-only, single-room signaling server for
// end-to-end tests of the full PacketDialer/PacketListener round trip
// (LISTEN/DIAL/INCOMING/READY/PEER).
//
// The real server (cmd/burrow-signal) is package main and cannot be
// imported as a library. UDP endpoints come from external STUN rather than
// from the TCP mapping lease, so UDP dials carry no token; the mock
// implements only the state machine the UDP path needs and performs no
// token check.
type mockUDPSignalServer struct {
	server *httptest.Server

	mu          sync.Mutex
	listener    *mockSignalPeer
	sessions    map[string]*mockSignalSession
	dialerPeers map[string]*mockSignalPeer
}

type mockSignalSession struct {
	dial  signalproto.Message
	ready *signalproto.Message
}

type mockSignalPeer struct {
	send func(signalproto.Message) error
}

func startMockUDPSignalServer(t *testing.T) *mockUDPSignalServer {
	t.Helper()
	m := &mockUDPSignalServer{
		sessions:    make(map[string]*mockSignalSession),
		dialerPeers: make(map[string]*mockSignalPeer),
	}
	mux := http.NewServeMux()
	// The client encodes signaling parameters into a single
	// "<prefix>?command=signal" URL (see dialSignalWebSocket and signalURL
	// in signal.go), and the prefix defaults to the root path. Register on
	// "/" to match how the real server routes commands, rather than on a
	// separate "/signal" path.
	mux.HandleFunc("/", m.handle)
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockUDPSignalServer) addr() string {
	return m.server.Listener.Addr().String()
}

func (m *mockUDPSignalServer) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(signalproto.MaxMessageBytes)

	var writeMu sync.Mutex
	peer := &mockSignalPeer{send: func(message signalproto.Message) error {
		data, err := signalproto.Encode(message)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return conn.Write(ctx, websocket.MessageText, data)
	}}

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			m.disconnect(peer)
			return
		}
		message, err := signalproto.Decode(data)
		if err != nil {
			continue
		}
		m.dispatch(peer, message)
	}
}

func (m *mockUDPSignalServer) disconnect(peer *mockSignalPeer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listener == peer {
		m.listener = nil
	}
}

func (m *mockUDPSignalServer) dispatch(peer *mockSignalPeer, message signalproto.Message) {
	switch message.Type {
	case signalproto.TypeListen:
		m.mu.Lock()
		m.listener = peer
		m.mu.Unlock()
	case signalproto.TypeDial:
		m.mu.Lock()
		listener := m.listener
		m.sessions[message.Session] = &mockSignalSession{dial: message}
		m.dialerPeers[message.Session] = peer
		m.mu.Unlock()
		if listener == nil {
			_ = peer.send(signalproto.Message{Type: signalproto.TypeError, Session: message.Session, Code: "no_listener"})
			return
		}
		incoming := message
		incoming.Type = signalproto.TypeIncoming
		_ = listener.send(incoming)
	case signalproto.TypeReady:
		m.mu.Lock()
		session, exists := m.sessions[message.Session]
		var dialerPeer *mockSignalPeer
		if exists {
			ready := message
			session.ready = &ready
			dialerPeer = m.dialerPeers[message.Session]
		}
		m.mu.Unlock()
		if !exists || dialerPeer == nil {
			return
		}
		peerMsg := message
		peerMsg.Type = signalproto.TypePeer
		_ = dialerPeer.send(peerMsg)
	}
}
