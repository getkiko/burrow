package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/signalproto"
)

func newRegistryPeer() *signalPeer {
	return &signalPeer{id: peerSequence.Add(1), remoteIP: "127.0.0.1", send: func([]byte) error { return nil }}
}

func dialMessage(room, session string) signalproto.Message {
	params := rawParams()
	return signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        room,
		Session:     session,
		Transport:   signalproto.TransportTCP,
		Strategy:    signalproto.StrategyPredictive,
		Parameters:  params,
		Fingerprint: params.FingerprintForTransport(signalproto.TransportTCP),
	}
}

func roomExists(r *registry, room string) bool {
	key := roomKey{transport: signalproto.TransportTCP, room: room}
	shard := r.shardFor(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	_, exists := shard.rooms[key]
	return exists
}

// Disconnect removes the peer from its own rooms and leaves other rooms
// alone.
func TestDisconnectOnlyTouchesPeerRooms(t *testing.T) {
	reg := newRegistry()
	now := time.Now()
	first, second, dialer := newRegistryPeer(), newRegistryPeer(), newRegistryPeer()
	if code := reg.Listen(signalproto.TransportTCP, "room-a", 0, first, now); code != "" {
		t.Fatalf("listen room-a: %s", code)
	}
	if code := reg.Listen(signalproto.TransportTCP, "room-b", 0, second, now); code != "" {
		t.Fatalf("listen room-b: %s", code)
	}
	if result := reg.Dial(dialMessage("room-b", "sess-1"), dialer, now); result.NotifyListener != second {
		t.Fatalf("dial room-b: %+v", result)
	}

	reg.Disconnect(first)
	if roomExists(reg, "room-a") {
		t.Fatal("room-a should be removed with its only listener")
	}
	if !roomExists(reg, "room-b") {
		t.Fatal("room-b belongs to another listener and must survive")
	}
	if got := first.roomCount(); got != 0 {
		t.Fatalf("disconnected peer still tracks %d rooms", got)
	}

	reg.Disconnect(dialer)
	if _, sessions := reg.Stats(); sessions != 1 {
		// The listener is still present, so the session stays until Sweep.
		t.Fatalf("sessions = %d, want 1", sessions)
	}
	reg.Disconnect(second)
	if rooms, sessions := reg.Stats(); rooms != 0 || sessions != 0 {
		t.Fatalf("registry should be empty, got rooms=%d sessions=%d", rooms, sessions)
	}
}

// One connection cannot register more than maxRoomsPerPeer rooms, but
// renewing a room it already holds always works.
func TestListenRoomLimitPerConnection(t *testing.T) {
	reg := newRegistry()
	reg.maxRoomsPerPeer = 2
	now := time.Now()
	peer := newRegistryPeer()
	for _, room := range []string{"room-1", "room-2"} {
		if code := reg.Listen(signalproto.TransportTCP, room, 0, peer, now); code != "" {
			t.Fatalf("listen %s: %s", room, code)
		}
	}
	if code := reg.Listen(signalproto.TransportTCP, "room-3", 0, peer, now); code != codeTooManyRooms {
		t.Fatalf("third room code = %q, want %q", code, codeTooManyRooms)
	}
	if roomExists(reg, "room-3") {
		t.Fatal("a rejected LISTEN must not create the room")
	}
	if code := reg.Listen(signalproto.TransportTCP, "room-1", 0, peer, now.Add(time.Second)); code != "" {
		t.Fatalf("renewing an existing room failed: %s", code)
	}

	// Once the peer no longer holds room-1 (another listener took it over),
	// the stale entry is pruned and a new room fits again.
	other := newRegistryPeer()
	if code := reg.Listen(signalproto.TransportTCP, "room-1", 0, other, now); code != "" {
		t.Fatalf("takeover: %s", code)
	}
	if code := reg.Listen(signalproto.TransportTCP, "room-3", 0, peer, now); code != "" {
		t.Fatalf("listen after prune: %s", code)
	}
}

// The room limit also applies to the rooms a connection dials into.
func TestDialRoomLimitPerConnection(t *testing.T) {
	reg := newRegistry()
	reg.maxRoomsPerPeer = 1
	now := time.Now()
	for _, room := range []string{"room-a", "room-b"} {
		if code := reg.Listen(signalproto.TransportTCP, room, 0, newRegistryPeer(), now); code != "" {
			t.Fatalf("listen %s: %s", room, code)
		}
	}
	dialer := newRegistryPeer()
	if result := reg.Dial(dialMessage("room-a", "sess-1"), dialer, now); result.Code != "" {
		t.Fatalf("first dial: %s", result.Code)
	}
	// More sessions in the same room are not limited by the room count.
	if result := reg.Dial(dialMessage("room-a", "sess-2"), dialer, now); result.Code != "" {
		t.Fatalf("second session in the same room: %s", result.Code)
	}
	if result := reg.Dial(dialMessage("room-b", "sess-3"), dialer, now); result.Code != codeTooManyRooms {
		t.Fatalf("dial into a second room: code = %q, want %q", result.Code, codeTooManyRooms)
	}
}

func TestRateLimiterPruneDropsIdleBuckets(t *testing.T) {
	limiter := newIPRateLimiter(10, 20) // refills completely in 2s
	now := time.Now()
	limiter.Allow("192.0.2.1", now)
	limiter.Allow("192.0.2.2", now.Add(1500*time.Millisecond))
	limiter.Prune(now.Add(time.Second))
	if got := limiter.size(); got != 2 {
		t.Fatalf("buckets = %d, want 2 before they refill", got)
	}
	limiter.Prune(now.Add(2 * time.Second))
	if got := limiter.size(); got != 1 {
		t.Fatalf("buckets = %d, want 1 after the first refilled", got)
	}
	limiter.Prune(now.Add(time.Minute))
	if got := limiter.size(); got != 0 {
		t.Fatalf("buckets = %d, want 0", got)
	}
}

// service.sweep also prunes both rate limiters.
func TestServiceSweepPrunesLimiters(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	now := time.Now()
	service.limiter.Allow("192.0.2.1", now)
	service.listenLimiter.Allow("192.0.2.1", now)
	service.sweep(now.Add(time.Hour))
	if service.limiter.size() != 0 || service.listenLimiter.size() != 0 {
		t.Fatal("sweep should drop idle rate-limit buckets")
	}
}

// LISTEN is rate limited per source IP, while normal renewals pass.
func TestListenRateLimited(t *testing.T) {
	_, url := newTestService(t)
	conn := dialTest(t, url)
	for range listenBurstPerIP * 4 {
		conn.send(signalproto.Message{Type: signalproto.TypeListen, Room: "room-flood"})
	}
	got := conn.recv()
	if got.Type != signalproto.TypeError || got.Code != "rate_limited" {
		t.Fatalf("flooding LISTEN should be rate limited, got %+v", got)
	}
}

func TestIPConnLimiter(t *testing.T) {
	limiter := newIPConnLimiter(2)
	if !limiter.Acquire("192.0.2.1") || !limiter.Acquire("192.0.2.1") {
		t.Fatal("first two slots should be granted")
	}
	if limiter.Acquire("192.0.2.1") {
		t.Fatal("third slot should be refused")
	}
	if !limiter.Acquire("192.0.2.2") {
		t.Fatal("another IP has its own slots")
	}
	limiter.Release("192.0.2.1")
	if !limiter.Acquire("192.0.2.1") {
		t.Fatal("a released slot should be reusable")
	}
	limiter.Release("192.0.2.1")
	limiter.Release("192.0.2.1")
	limiter.Release("192.0.2.2")
	if len(limiter.conns) != 0 {
		t.Fatalf("released IPs should be forgotten, got %v", limiter.conns)
	}
	if unlimited := newIPConnLimiter(0); !unlimited.Acquire("192.0.2.1") {
		t.Fatal("a zero limit disables the check")
	}
}

// /nat connections beyond the per-IP limit are refused with 429, and a slot
// frees up when a lease closes.
func TestNATProbePerIPLimit(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	service.natConns = newIPConnLimiter(2)
	server := httptest.NewServer(http.HandlerFunc(service.handleNATProbeWebSocket))
	t.Cleanup(server.Close)
	url := "ws" + server.URL[len("http"):] + "?room=room-nat&session=sess-nat"

	open := func() (*websocket.Conn, *http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, response, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			// Wait for the probe response so the lease is fully established.
			if _, _, err = conn.Read(ctx); err != nil {
				t.Fatalf("read probe response: %v", err)
			}
		}
		return conn, response, err
	}

	first, _, err := open()
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	second, _, err := open()
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	defer second.CloseNow()
	_, response, err := open()
	if err == nil {
		t.Fatal("third concurrent lease should be refused")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third lease: response = %v, err = %v; want HTTP 429", response, err)
	}

	_ = first.Close(websocket.StatusNormalClosure, "done")
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, _, err := open()
		if err == nil {
			conn.CloseNow()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot was not released after closing a lease: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLimitKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"192.0.2.10", "192.0.2.10"},
		{"::ffff:192.0.2.10", "192.0.2.10"},
		{"2001:db8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::/64"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"fe80::1%eth0", "fe80::/64"},
		{"::1", "::/64"},
	}
	for _, test := range tests {
		if got := limitKey(test.in); got != test.want {
			t.Fatalf("limitKey(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

// Rotating addresses inside one IPv6 /64 must not bypass the LISTEN limit,
// while the full address is still what the token check sees.
func TestListenRateLimitSharedAcrossIPv6Prefix(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	var limited int
	for i := range listenBurstPerIP + 5 {
		ip := fmt.Sprintf("2001:db8:1:2::%x", i+1)
		peer := &signalPeer{id: peerSequence.Add(1), remoteIP: ip, limitKey: limitKey(ip)}
		peer.send = func(data []byte) error {
			message, err := signalproto.Decode(data)
			if err == nil && message.Code == "rate_limited" {
				limited++
			}
			return nil
		}
		service.handleSignalMessage(peer, signalproto.Message{Type: signalproto.TypeListen, Room: fmt.Sprintf("room-%d", i)})
		if peer.remoteIP != ip {
			t.Fatalf("peer address rewritten to %q", peer.remoteIP)
		}
	}
	if limited == 0 {
		t.Fatal("addresses from one /64 should share a LISTEN bucket")
	}

	// A different /64 has its own bucket.
	other := "2001:db8:1:3::1"
	peer := &signalPeer{id: peerSequence.Add(1), remoteIP: other, limitKey: limitKey(other)}
	var rejected bool
	peer.send = func([]byte) error { rejected = true; return nil }
	service.handleSignalMessage(peer, signalproto.Message{Type: signalproto.TypeListen, Room: "room-other"})
	if rejected {
		t.Fatal("another /64 must not be limited")
	}
}

func TestIPConnLimiterSharedAcrossIPv6Prefix(t *testing.T) {
	limiter := newIPConnLimiter(1)
	if !limiter.Acquire(limitKey("2001:db8::1")) {
		t.Fatal("first connection should be granted")
	}
	if limiter.Acquire(limitKey("2001:db8::2")) {
		t.Fatal("a second address in the same /64 should share the slot")
	}
	if !limiter.Acquire(limitKey("2001:db8:0:1::1")) {
		t.Fatal("another /64 should have its own slot")
	}
}

func newSignalTestServer(t *testing.T, service *signalService) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(service.handleSignalWebSocket))
	t.Cleanup(server.Close)
	return "ws" + server.URL[len("http"):]
}

// /signal connections beyond the per-source limit are refused with 429.
func TestSignalPerIPLimit(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	service.signalConns = newIPConnLimiter(2)
	url := newSignalTestServer(t, service)
	first := dialTest(t, url)
	_ = dialTest(t, url)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		conn.CloseNow()
		t.Fatal("third concurrent signal connection should be refused")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("response = %v, err = %v; want HTTP 429", response, err)
	}

	_ = first.conn.Close(websocket.StatusNormalClosure, "done")
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, _, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			conn.CloseNow()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot was not released: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An idle /signal connection is closed after idleTimeout.
func TestSignalIdleTimeoutClosesIdleConnection(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	service.idleTimeout = 200 * time.Millisecond
	conn := dialTest(t, newSignalTestServer(t, service))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, _, err := conn.conn.Read(ctx); err == nil {
		t.Fatal("expected the server to close the idle connection")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("idle connection closed after %s, want about 200ms", elapsed)
	}
}

// Messages sent more often than idleTimeout, like LISTEN renewals, keep the
// connection open well past the timeout.
func TestSignalIdleTimeoutSparesActiveConnection(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	service.idleTimeout = 300 * time.Millisecond
	conn := dialTest(t, newSignalTestServer(t, service))
	for range 10 {
		conn.send(signalproto.Message{Type: signalproto.TypeListen, Room: "room-renew"})
		time.Sleep(100 * time.Millisecond)
	}
	// The connection is still served: an invalid message gets an answer.
	conn.send(signalproto.Message{Type: signalproto.TypeListen, Room: ""})
	if got := conn.recv(); got.Code != "invalid_room" {
		t.Fatalf("connection should still be open, got %+v", got)
	}
}

// The default idle timeout leaves ample room for the SDK's renewal (5s)
// and resend (0.5s) intervals.
func TestDefaultSignalIdleTimeoutCoversRenewals(t *testing.T) {
	const sdkListenRenewInterval = 5 * time.Second
	if defaultSignalIdleTimeout < 6*sdkListenRenewInterval {
		t.Fatalf("idle timeout %s is too close to the renewal interval %s", defaultSignalIdleTimeout, sdkListenRenewInterval)
	}
}
