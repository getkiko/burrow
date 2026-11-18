package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/signalproto"
)

const (
	websocketWriteTimeout = 5 * time.Second
	natProbeHoldTimeout   = 30 * time.Minute

	// A listener renews its room every 5 seconds (0.2/s), so the LISTEN
	// limit leaves room for many listeners behind one address (for example a
	// carrier-grade NAT) while bounding registry churn from a single source.
	listenRatePerIP  = 20
	listenBurstPerIP = 40

	// defaultMaxSignalPerIP bounds concurrent /signal connections per source
	// (an IPv4 address or an IPv6 /64). The SDK opens one connection per
	// listener or dialer, so the default leaves room for many clients behind
	// one egress address such as a carrier-grade NAT.
	defaultMaxSignalPerIP = 256

	// defaultSignalIdleTimeout closes a /signal connection that sends no
	// message for this long. A listener renews every 5 seconds and a waiting
	// dialer resends every half second, so only idle connections are
	// affected; an idle SDK client reconnects on its next use.
	defaultSignalIdleTimeout = 60 * time.Second

	// defaultMaxNATPerIP bounds concurrent /nat mapping leases per source IP.
	// A client holds one or two leases per punching attempt, and they close
	// when the attempt ends.
	defaultMaxNATPerIP = 64
)

var peerSequence atomic.Uint64

type signalService struct {
	registry *registry
	tokens   *tokenSigner
	// limiter rate-limits DIAL per source IP; listenLimiter does the same
	// for LISTEN.
	limiter       *ipRateLimiter
	listenLimiter *ipRateLimiter
	// natConns and signalConns cap concurrent /nat and /signal connections
	// per source. Every per-source limit is keyed by limitKey, not the full
	// address.
	natConns    *ipConnLimiter
	signalConns *ipConnLimiter
	// idleTimeout closes a /signal connection after this long without a
	// message; zero or less disables it.
	idleTimeout time.Duration
}

func newSignalService(registry *registry, secret []byte) *signalService {
	return &signalService{
		registry:      registry,
		tokens:        newTokenSigner(secret),
		limiter:       newIPRateLimiter(10, 20),
		listenLimiter: newIPRateLimiter(listenRatePerIP, listenBurstPerIP),
		natConns:      newIPConnLimiter(defaultMaxNATPerIP),
		signalConns:   newIPConnLimiter(defaultMaxSignalPerIP),
		idleTimeout:   defaultSignalIdleTimeout,
	}
}

// sweep expires stale registry entries and idle rate-limit buckets.
func (s *signalService) sweep(now time.Time) {
	s.registry.Sweep(now)
	s.limiter.Prune(now)
	s.listenLimiter.Prune(now)
}

func serveWebSocketSignal(listen, prefix string, service *signalService) error {
	mux := http.NewServeMux()
	// Main entry point: the configured prefix path, dispatched by ?command=<name>.
	// An empty prefix means the root path.
	mux.HandleFunc(normalizePrefix(prefix), service.handleCommand)

	mux.HandleFunc("/signal", service.handleSignalWebSocket)
	mux.HandleFunc("/probe", service.handleProbeWebSocket)
	mux.HandleFunc("/echo", service.handleProbeWebSocket)
	mux.HandleFunc("/nat", service.handleNATProbeWebSocket)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("[ws] signal v%d ready: ws://%s%s?command={signal|echo|nat}", signalproto.Version, listen, normalizePrefix(prefix))
	return server.ListenAndServe()
}

// normalizePrefix turns the configured prefix into an exact mux path. An empty
// prefix becomes "/"; otherwise a leading slash is added and trailing slashes
// are removed.
func normalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "/"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return prefix
}

// effectiveQuery returns the signaling parameters of a request. Both inputs are
// always accepted: the hidden cookie (a base64-encoded query) wins, and if it is
// missing or fails to decode the plain URL query is used. The fallback keeps
// clients that do not use the cookie, and requests where a middlebox injected a
// garbage cookie, working.
func effectiveQuery(r *http.Request) url.Values {
	if cookie, err := r.Cookie(signalproto.HideCookieName); err == nil {
		if values, decodeErr := signalproto.DecodeHiddenQuery(cookie.Value); decodeErr == nil && len(values) > 0 {
			return values
		}
	}
	return r.URL.Query()
}

// handleCommand routes a request to the handler selected by its command.
func (s *signalService) handleCommand(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimSpace(effectiveQuery(r).Get(signalproto.CommandKey)) {
	case signalproto.CommandSignal:
		s.handleSignalWebSocket(w, r)
	case signalproto.CommandEcho:
		s.handleProbeWebSocket(w, r)
	case signalproto.CommandNAT:
		s.handleNATProbeWebSocket(w, r)
	case signalproto.CommandHealth:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (s *signalService) handleSignalWebSocket(w http.ResponseWriter, r *http.Request) {
	remoteIP, err := remoteIP(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid remote address", http.StatusBadRequest)
		return
	}
	limitKey := limitKey(remoteIP)
	if !s.signalConns.Acquire(limitKey) {
		metrics.rateLimited.Add(1)
		http.Error(w, "too many concurrent signal connections", http.StatusTooManyRequests)
		return
	}
	defer s.signalConns.Release(limitKey)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	peer := newSignalPeer(conn, remoteIP)
	defer func() {
		s.registry.Disconnect(peer)
		_ = conn.Close(websocket.StatusNormalClosure, "closed")
	}()
	conn.SetReadLimit(signalproto.MaxMessageBytes)

	for {
		messageType, data, err := s.readMessage(r.Context(), conn)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		message, err := parseMessage(data)
		if err != nil {
			metrics.badPacket.Add(1)
			_ = peer.send(encodeError("", "bad_message", err.Error()))
			continue
		}
		s.handleSignalMessage(peer, message)
	}
}

// readMessage reads the next message, giving up after idleTimeout. Control
// frames such as pings are handled inside Read and do not reset the timer;
// only real messages (LISTEN renewals, DIAL resends) keep a connection open.
func (s *signalService) readMessage(ctx context.Context, conn *websocket.Conn) (websocket.MessageType, []byte, error) {
	if s.idleTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.idleTimeout)
		defer cancel()
	}
	return conn.Read(ctx)
}

func (s *signalService) handleSignalMessage(peer *signalPeer, message signalproto.Message) {
	now := time.Now()
	switch message.Type {
	case signalproto.TypeListen:
		if !s.listenLimiter.Allow(peer.limitKey, now) {
			metrics.rateLimited.Add(1)
			_ = peer.send(encodeError("", "rate_limited", "listen rate exceeded"))
			return
		}
		if !validName(message.Room, signalproto.MaxRoomLength) {
			_ = peer.send(encodeError("", "invalid_room", "invalid room"))
			return
		}
		if code := s.registry.Listen(message.Transport, message.Room, message.Capacity, peer, now); code != "" {
			_ = peer.send(encodeError("", code, "too many rooms on this connection"))
			return
		}
		metrics.listen.Add(1)
	case signalproto.TypeDial:
		metrics.dial.Add(1)
		if !s.limiter.Allow(peer.limitKey, now) {
			metrics.rateLimited.Add(1)
			_ = peer.send(encodeError(message.Session, "rate_limited", "dial rate exceeded"))
			return
		}
		if !validName(message.Room, signalproto.MaxRoomLength) || !validName(message.Session, signalproto.MaxSessionLength) || signalproto.ValidateEndpoint(message.Endpoint) != nil || signalproto.ValidateStrategy(message.Strategy) != nil {
			_ = peer.send(encodeError(message.Session, "invalid_dial", "invalid room, session, or endpoint"))
			return
		}
		if err := validateProof(message); err != nil {
			_ = peer.send(encodeError(message.Session, "invalid_proof", err.Error()))
			return
		}
		// A UDP endpoint comes from an external STUN server, not from a /nat
		// mapping lease, so there is no mapping token to verify. Its
		// authenticity is covered by the proof instead (validateProof above
		// requires one for UDP). TCP always verifies the token.
		if message.Transport != signalproto.TransportUDP {
			if err := s.tokens.Verify(message.Token, message.Endpoint, message.Room, message.Session, peer.remoteIP, now); err != nil {
				metrics.tokenRejected.Add(1)
				_ = peer.send(encodeError(message.Session, "invalid_token", err.Error()))
				return
			}
		}
		if message.Fingerprint == "" || message.Fingerprint != message.Parameters.FingerprintForTransport(message.Transport) {
			_ = peer.send(encodeError(message.Session, "strategy_mismatch", "invalid strategy fingerprint"))
			return
		}
		result := s.registry.Dial(message, peer, now)
		switch {
		case result.Code != "":
			_ = peer.send(encodeError(message.Session, result.Code, result.Code))
			if result.Code == "no_listener" {
				metrics.noRoom.Add(1)
			}
		case result.Peer != nil:
			_ = peer.send(encodeMessage(*result.Peer))
			metrics.peer.Add(1)
			metrics.peerResent.Add(1)
		case result.Error != nil:
			_ = peer.send(encodeMessage(*result.Error))
		case result.NotifyListener != nil:
			_ = result.NotifyListener.send(encodeMessage(result.Incoming))
			metrics.incoming.Add(1)
		}
	case signalproto.TypeReady:
		metrics.ready.Add(1)
		if signalproto.ValidateEndpoint(message.Endpoint) != nil || signalproto.ValidateStrategy(message.Strategy) != nil {
			_ = peer.send(encodeError(message.Session, "invalid_ready", "invalid ready message"))
			return
		}
		if err := validateProof(message); err != nil {
			_ = peer.send(encodeError(message.Session, "invalid_proof", err.Error()))
			return
		}
		result := s.registry.Ready(message, peer)
		if result.Code != "" {
			_ = peer.send(encodeError(message.Session, result.Code, result.Code))
			return
		}
		_ = result.Dialer.send(encodeMessage(result.Peer))
		metrics.peer.Add(1)
	case signalproto.TypeError:
		if !validName(message.Room, signalproto.MaxRoomLength) || !validName(message.Session, signalproto.MaxSessionLength) || !validName(message.Code, signalproto.MaxCodeLength) {
			_ = peer.send(encodeError(message.Session, "invalid_error", "invalid error message"))
			return
		}
		result := s.registry.Reject(message, peer)
		if result.Code != "" {
			_ = peer.send(encodeError(message.Session, result.Code, result.Code))
			return
		}
		_ = result.Dialer.send(encodeMessage(message))
	default:
		_ = peer.send(encodeError(message.Session, "invalid_type", "unexpected client message type"))
	}
}

// validateProof enforces the proof rule: a udp message without a proof is
// rejected, because UDP has no probe token and relies on the proof to
// authenticate signaling. A tcp message is authenticated by its token, so a
// missing proof is accepted. message.Transport was normalized by Decode.
func validateProof(message signalproto.Message) error {
	if message.Transport == signalproto.TransportUDP && message.Proof == "" {
		return errors.New("udp signal message missing proof")
	}
	return nil
}

func newSignalPeer(conn *websocket.Conn, remoteIP string) *signalPeer {
	peer := &signalPeer{remoteIP: remoteIP, limitKey: limitKey(remoteIP)}
	var writeMu sync.Mutex
	peer.id = peerSequence.Add(1)
	peer.send = func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), websocketWriteTimeout)
		defer cancel()
		return conn.Write(ctx, websocket.MessageText, data)
	}
	return peer
}

func (s *signalService) handleProbeWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, ok := s.writeProbeResponse(w, r)
	if ok {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
}

func (s *signalService) handleNATProbeWebSocket(w http.ResponseWriter, r *http.Request) {
	// A /nat connection is held open for up to natProbeHoldTimeout, so cap
	// how many one source IP may hold at once. The check runs before the
	// WebSocket upgrade, so a rejected client gets a plain HTTP 429.
	ip, err := remoteIP(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid remote address", http.StatusBadRequest)
		return
	}
	key := limitKey(ip)
	if !s.natConns.Acquire(key) {
		metrics.rateLimited.Add(1)
		http.Error(w, "too many concurrent nat probes", http.StatusTooManyRequests)
		return
	}
	defer s.natConns.Release(key)
	conn, ok := s.writeProbeResponse(w, r)
	if !ok {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(signalproto.MaxMessageBytes)
	ctx, cancel := context.WithTimeout(r.Context(), natProbeHoldTimeout)
	defer cancel()
	_, _, _ = conn.Read(ctx)
}

func (s *signalService) writeProbeResponse(w http.ResponseWriter, r *http.Request) (*websocket.Conn, bool) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, false
	}
	query := effectiveQuery(r)
	room := strings.TrimSpace(query.Get("room"))
	session := strings.TrimSpace(query.Get("session"))
	if !validName(room, signalproto.MaxRoomLength) || !validName(session, signalproto.MaxSessionLength) {
		_ = conn.Close(websocket.StatusPolicyViolation, "missing room or session")
		return nil, false
	}
	endpoint := strings.TrimSpace(r.RemoteAddr)
	if signalproto.ValidateEndpoint(endpoint) != nil {
		_ = conn.Close(websocket.StatusInternalError, "invalid remote address")
		return nil, false
	}
	token, expires := s.tokens.Issue(endpoint, room, session, time.Now())
	response, _ := json.Marshal(signalproto.ProbeResponse{
		Version:   signalproto.Version,
		Endpoint:  endpoint,
		Token:     token,
		ExpiresAt: expires,
	})
	ctx, cancel := context.WithTimeout(r.Context(), websocketWriteTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, response); err != nil && !errors.Is(err, context.Canceled) {
		_ = conn.Close(websocket.StatusInternalError, "write probe response failed")
		return nil, false
	}
	return conn, true
}

func remoteIP(address string) (string, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil || net.ParseIP(host) == nil {
		return "", errors.New("invalid remote address")
	}
	return host, nil
}

// validName applies the shared name rule from signalproto, which clients
// also use to validate their configuration.
func validName(value string, max int) bool {
	return signalproto.ValidName(value, max)
}
