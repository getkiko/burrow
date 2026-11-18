package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/signalproto"
)

// newTestService starts a real signal WebSocket test server around
// handleSignalWebSocket for end-to-end tests.
func newTestService(t *testing.T) (*signalService, string) {
	t.Helper()
	service := newSignalService(newRegistry(), []byte("signal-test-secret"))
	server := httptest.NewServer(http.HandlerFunc(service.handleSignalWebSocket))
	t.Cleanup(server.Close)
	return service, "ws" + server.URL[len("http"):]
}

type testConn struct {
	t    *testing.T
	conn *websocket.Conn
}

func dialTest(t *testing.T, url string) *testConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	c := &testConn{t: t, conn: conn}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "test done") })
	return c
}

// send writes message as is. A zero Version is replaced by
// signalproto.Version; any other value is sent unchanged so tests can
// exercise version checks.
func (c *testConn) send(message signalproto.Message) {
	c.t.Helper()
	if message.Version == 0 {
		message.Version = signalproto.Version
	}
	data, err := json.Marshal(message)
	if err != nil {
		c.t.Fatalf("encode failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		c.t.Fatalf("write failed: %v", err)
	}
}

func (c *testConn) recv() signalproto.Message {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read failed: %v", err)
	}
	message, err := signalproto.Decode(data)
	if err != nil {
		c.t.Fatalf("decode failed: %v (raw=%s)", err, data)
	}
	return message
}

// rawParams returns minimal strategy parameters shared by both sides so the
// fingerprint check passes.
func rawParams() signalproto.StrategyParameters {
	return signalproto.StrategyParameters{PredictiveRange: 64}
}

// A full TCP exchange (LISTEN, DIAL, INCOMING, READY, PEER) succeeds and every
// reply carries the current protocol version.
func TestTCPFullFlow(t *testing.T) {
	service, url := newTestService(t)

	listener := dialTest(t, url)
	listener.send(signalproto.Message{Type: signalproto.TypeListen, Room: "room-flow", Transport: signalproto.TransportTCP})
	time.Sleep(50 * time.Millisecond) // let the registry record the listener

	dialer := dialTest(t, url)
	params := rawParams()
	token, _ := service.tokens.Issue("127.0.0.1:1", "room-flow", "sess-flow", time.Now())
	dialer.send(signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        "room-flow",
		Session:     "sess-flow",
		Endpoint:    "127.0.0.1:1",
		Token:       token,
		Transport:   signalproto.TransportTCP,
		Strategy:    signalproto.StrategyPredictive,
		Parameters:  params,
		Fingerprint: params.FingerprintForTransport(signalproto.TransportTCP),
	})

	incoming := listener.recv()
	if incoming.Type != signalproto.TypeIncoming {
		t.Fatalf("listener should receive incoming, got %+v", incoming)
	}
	if incoming.Version != signalproto.Version {
		t.Fatalf("incoming version should be %d, got %d", signalproto.Version, incoming.Version)
	}

	listener.send(signalproto.Message{
		Type:        signalproto.TypeReady,
		Room:        "room-flow",
		Session:     "sess-flow",
		Endpoint:    "127.0.0.1:2",
		Transport:   signalproto.TransportTCP,
		Strategy:    signalproto.StrategyPredictive,
		Parameters:  params,
		Fingerprint: params.FingerprintForTransport(signalproto.TransportTCP),
	})

	peerMsg := dialer.recv()
	if peerMsg.Type != signalproto.TypePeer {
		t.Fatalf("dialer should receive peer, got %+v", peerMsg)
	}
	if peerMsg.Version != signalproto.Version {
		t.Fatalf("peer version should be %d, got %d", signalproto.Version, peerMsg.Version)
	}
	if peerMsg.Endpoint != "127.0.0.1:2" {
		t.Fatalf("peer endpoint should be the listener's, got %q", peerMsg.Endpoint)
	}
}

// A peer that omits transport must pair with one that sends "tcp": Decode
// normalizes the field so the room key is not split between "" and "tcp".
func TestOmittedTransportPairsWithTCP(t *testing.T) {
	service, url := newTestService(t)

	// The listener declares transport=tcp explicitly.
	listener := dialTest(t, url)
	listener.send(signalproto.Message{Type: signalproto.TypeListen, Room: "shared-room", Transport: signalproto.TransportTCP})
	time.Sleep(50 * time.Millisecond)

	// The dialer omits transport.
	dialer := dialTest(t, url)
	params := rawParams()
	token, _ := service.tokens.Issue("127.0.0.1:1", "shared-room", "sess-mix", time.Now())
	dialer.send(signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        "shared-room",
		Session:     "sess-mix",
		Endpoint:    "127.0.0.1:1",
		Token:       token,
		Strategy:    signalproto.StrategyPredictive,
		Parameters:  params,
		Fingerprint: params.Fingerprint(),
	})

	incoming := listener.recv()
	if incoming.Type != signalproto.TypeIncoming || incoming.Session != "sess-mix" {
		t.Fatalf("listener should receive the dialer's incoming, got %+v", incoming)
	}
}

// A LISTEN without capacity falls back to maxSessionsPerRoom: once that many
// sessions are pending (neither ready nor rejected), the next one gets
// room_busy.
func TestListenWithoutCapacityUsesDefault(t *testing.T) {
	service, url := newTestService(t)

	listener := dialTest(t, url)
	listener.send(signalproto.Message{Type: signalproto.TypeListen, Room: "room-cap"})
	time.Sleep(50 * time.Millisecond)

	params := rawParams()
	for i := range maxSessionsPerRoom {
		dialer := dialTest(t, url)
		session := "sess-cap-" + string(rune('a'+i))
		token, _ := service.tokens.Issue("127.0.0.1:1", "room-cap", session, time.Now())
		dialer.send(signalproto.Message{
			Type:        signalproto.TypeDial,
			Room:        "room-cap",
			Session:     session,
			Endpoint:    "127.0.0.1:1",
			Token:       token,
			Strategy:    signalproto.StrategyPredictive,
			Parameters:  params,
			Fingerprint: params.Fingerprint(),
		})
		got := listener.recv()
		if got.Type != signalproto.TypeIncoming {
			t.Fatalf("session %d should be accepted, got %+v", i, got)
		}
	}

	// One more session exceeds the default capacity.
	overflow := dialTest(t, url)
	token, _ := service.tokens.Issue("127.0.0.1:1", "room-cap", "sess-cap-overflow", time.Now())
	overflow.send(signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        "room-cap",
		Session:     "sess-cap-overflow",
		Endpoint:    "127.0.0.1:1",
		Token:       token,
		Strategy:    signalproto.StrategyPredictive,
		Parameters:  params,
		Fingerprint: params.Fingerprint(),
	})
	errMsg := overflow.recv()
	if errMsg.Type != signalproto.TypeError || errMsg.Code != "room_busy" {
		t.Fatalf("exceeding the default capacity should yield room_busy, got %+v", errMsg)
	}
}

// A udp DIAL without a proof must be rejected.
func TestUDPDialWithoutProofRejected(t *testing.T) {
	_, url := newTestService(t)

	dialer := dialTest(t, url)
	params := rawParams()
	dialer.send(signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        "room-udp",
		Session:     "sess-udp",
		Endpoint:    "127.0.0.1:1",
		Transport:   signalproto.TransportUDP,
		Strategy:    signalproto.StrategyPredictive,
		Parameters:  params,
		Fingerprint: params.FingerprintForTransport(signalproto.TransportUDP),
		// Proof deliberately empty.
	})
	errMsg := dialer.recv()
	if errMsg.Type != signalproto.TypeError || errMsg.Code != "invalid_proof" {
		t.Fatalf("udp without proof should be rejected, got %+v", errMsg)
	}
}

// A tcp DIAL without a proof must be accepted; tcp is authenticated by the
// probe token.
func TestTCPDialWithoutProofAccepted(t *testing.T) {
	service, url := newTestService(t)

	listener := dialTest(t, url)
	listener.send(signalproto.Message{Type: signalproto.TypeListen, Room: "room-tcp-noproof", Transport: signalproto.TransportTCP})
	time.Sleep(50 * time.Millisecond)

	dialer := dialTest(t, url)
	params := rawParams()
	token, _ := service.tokens.Issue("127.0.0.1:1", "room-tcp-noproof", "sess-tcp-noproof", time.Now())
	dialer.send(signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        "room-tcp-noproof",
		Session:     "sess-tcp-noproof",
		Endpoint:    "127.0.0.1:1",
		Token:       token,
		Transport:   signalproto.TransportTCP,
		Strategy:    signalproto.StrategyPredictive,
		Parameters:  params,
		Fingerprint: params.FingerprintForTransport(signalproto.TransportTCP),
		// Proof deliberately empty; tcp should be accepted.
	})
	incoming := listener.recv()
	if incoming.Type != signalproto.TypeIncoming {
		t.Fatalf("tcp without proof should be accepted, got %+v", incoming)
	}
}

// Any version other than signalproto.Version is rejected with bad_message.
func TestUnsupportedVersionRejectedByServer(t *testing.T) {
	_, url := newTestService(t)
	for _, version := range []int{signalproto.Version + 1, 4, 5, -1} {
		conn := dialTest(t, url)
		conn.send(signalproto.Message{Version: version, Type: signalproto.TypeListen, Room: "room-bad-version"})
		errMsg := conn.recv()
		if errMsg.Type != signalproto.TypeError || errMsg.Code != "bad_message" {
			t.Fatalf("version=%d should be rejected as bad_message, got %+v", version, errMsg)
		}
	}
}

// A udp DIAL with a proof but no mapping token must be accepted. The UDP
// endpoint comes from an external STUN server rather than a /nat mapping
// lease, so it never has a token; verifying the token for every transport
// would wrongly reject valid UDP dials with invalid_token.
func TestUDPDialWithProofButNoTokenAccepted(t *testing.T) {
	_, url := newTestService(t)

	listener := dialTest(t, url)
	listener.send(signalproto.Message{
		Type:      signalproto.TypeListen,
		Room:      "room-udp-notoken",
		Transport: signalproto.TransportUDP,
	})
	time.Sleep(50 * time.Millisecond) // let the registry record the listener

	dialer := dialTest(t, url)
	params := rawParams()
	dialer.send(signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        "room-udp-notoken",
		Session:     "sess-udp-notoken",
		Endpoint:    "203.0.113.7:41234", // observed via an external STUN server
		Transport:   signalproto.TransportUDP,
		Proof:       "test-proof",
		Strategy:    signalproto.StrategyPredictive,
		Parameters:  params,
		Fingerprint: params.FingerprintForTransport(signalproto.TransportUDP),
		// Token deliberately empty: UDP has no mapping token.
	})

	incoming := listener.recv()
	if incoming.Type != signalproto.TypeIncoming {
		t.Fatalf("udp with proof and no token should be accepted, got %+v", incoming)
	}
	if incoming.Transport != signalproto.TransportUDP {
		t.Fatalf("incoming transport should be udp, got %q", incoming.Transport)
	}
}
