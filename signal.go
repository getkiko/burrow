package burrow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/signalproto"
)

var errSignalClosed = errors.New("burrow: signal connection closed")

// signalModeHeader is the Config.Mode value that moves signaling parameters
// into a Cookie.
const signalModeHeader = "header"

// dialResendInterval is how often the Dialer resends DIAL while waiting to
// be paired.
//
// It directly affects the duration of an attempt. After sending DIAL the
// Dialer blocks waiting for PEER, which the Listener only sends after a chain
// of work: acquireSession, opening a fresh WebSocket mapping lease (often 1-2
// seconds on long-haul or mobile networks), creating all candidate sockets
// and waiting out Warmup. Whenever that takes longer than this interval, the
// Dialer sleeps another full period before resending, so the pairing time is
// quantized to multiples of this interval: with 2 seconds, a pairing that
// should take 2.3 seconds takes 4.
//
// 500ms cuts that granularity to a quarter: at worst 0.5 seconds extra
// instead of 2. Resending is idempotent: the server's registry.Dial returns
// the cached result for an existing session without notifying the Listener
// again or starting a second attempt, so resending only adds signal server
// load and does not affect correctness.
//
// It is not smaller because resend frequency scales signal server load, and
// the real pairing bottleneck is the Listener's mapping lease setup; going
// far below that only spins.
const dialResendInterval = 500 * time.Millisecond

type incoming struct {
	message signalproto.Message
}

type mappingLease struct {
	Endpoint string
	Token    string
	conn     *websocket.Conn
	once     sync.Once
}

func (l *mappingLease) Close() error {
	if l == nil {
		return nil
	}
	var err error
	l.once.Do(func() {
		err = l.conn.Close(websocket.StatusNormalClosure, "mapping lease complete")
	})
	return err
}

type signalClient struct {
	addr       string
	mode       string
	control    func(network, address string, c syscall.RawConn) error
	resolveIP  ResolveIPFunc
	tcpNetwork string
}

func (s *signalClient) openMappingLease(ctx context.Context, target, localAddr, room, session string) (*mappingLease, error) {
	query := url.Values{"room": []string{room}, "session": []string{session}}
	conn, err := dialSignalWebSocket(ctx, target, signalproto.CommandNAT, query, s.mode, s.control, s.resolveIP, localAddr, s.tcpNetwork)
	if err != nil {
		return nil, fmt.Errorf("connect signal mapping probe: %w", err)
	}
	messageType, data, err := conn.Read(ctx)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "probe read failed")
		return nil, fmt.Errorf("read signal mapping probe: %w", err)
	}
	if messageType != websocket.MessageText {
		_ = conn.Close(websocket.StatusUnsupportedData, "invalid probe response")
		return nil, fmt.Errorf("signal mapping probe: unexpected message type %v", messageType)
	}
	var response signalproto.ProbeResponse
	if err := json.Unmarshal(data, &response); err != nil {
		_ = conn.Close(websocket.StatusUnsupportedData, "invalid probe response")
		return nil, fmt.Errorf("parse signal mapping probe: %w", err)
	}
	if response.Version != signalproto.Version || signalproto.ValidateEndpoint(response.Endpoint) != nil || response.Token == "" {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid probe response")
		return nil, errors.New("signal mapping probe: invalid response")
	}
	return &mappingLease{Endpoint: response.Endpoint, Token: response.Token, conn: conn}, nil
}

type signalResponse struct {
	message signalproto.Message
	err     error
}

type signalControl struct {
	addr       string
	room       string
	mode       string
	control    func(network, address string, c syscall.RawConn) error
	resolveIP  ResolveIPFunc
	tcpNetwork string

	mu          sync.Mutex
	conn        *websocket.Conn
	connDone    chan struct{}
	connecting  bool
	connectDone chan struct{}
	closed      bool
	generation  uint64
	writeMu     sync.Mutex
	pending     map[string]chan signalResponse
	incomingCh  chan incoming
	// listenErrCh carries errors the server sends for LISTEN. They have no
	// session, so deliver cannot route them; the listener's reconnect loop
	// reads them instead. It is buffered and written without blocking, so a
	// Dialer, which never reads it, is unaffected.
	listenErrCh chan signalproto.Message
}

func newSignalControl(addr, room, mode string, control func(network, address string, c syscall.RawConn) error, resolveIP ResolveIPFunc, queue int, tcpNetwork string) *signalControl {
	return &signalControl{
		addr:        addr,
		room:        room,
		mode:        mode,
		control:     control,
		resolveIP:   resolveIP,
		tcpNetwork:  tcpNetwork,
		pending:     make(map[string]chan signalResponse),
		incomingCh:  make(chan incoming, queue),
		listenErrCh: make(chan signalproto.Message, 1),
	}
}

func (s *signalControl) ensureConnected(ctx context.Context) (*websocket.Conn, <-chan struct{}, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, nil, errSignalClosed
		}
		if s.conn != nil {
			conn, done := s.conn, s.connDone
			s.mu.Unlock()
			return conn, done, nil
		}
		if s.connecting {
			wait := s.connectDone
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		s.connecting = true
		s.connectDone = make(chan struct{})
		wait := s.connectDone
		generation := s.generation
		s.mu.Unlock()

		conn, err := dialSignalWebSocket(ctx, s.addr, signalproto.CommandSignal, nil, s.mode, s.control, s.resolveIP, "", s.tcpNetwork)
		s.mu.Lock()
		s.connecting = false
		if err == nil && !s.closed && generation == s.generation {
			s.conn = conn
			s.connDone = make(chan struct{})
			done := s.connDone
			close(wait)
			s.mu.Unlock()
			conn.SetReadLimit(signalproto.MaxMessageBytes)
			go s.readLoop(conn, done)
			return conn, done, nil
		}
		close(wait)
		closed := s.closed
		networkChanged := generation != s.generation
		s.mu.Unlock()
		if conn != nil {
			_ = conn.Close(websocket.StatusGoingAway, "signal connect failed")
		}
		if closed {
			return nil, nil, errSignalClosed
		}
		if networkChanged {
			continue
		}
		return nil, nil, err
	}
}

func (s *signalControl) readLoop(conn *websocket.Conn, done chan struct{}) {
	for {
		messageType, data, err := conn.Read(context.Background())
		if err != nil {
			s.invalidate(conn, done)
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		message, err := signalproto.Decode(data)
		if err != nil {
			continue
		}
		switch message.Type {
		case signalproto.TypePeer:
			if signalproto.ValidateEndpoint(message.Endpoint) == nil {
				s.deliver(signalResponse{message: message})
			}
		case signalproto.TypeError:
			if isListenRejection(message) {
				select {
				case s.listenErrCh <- message:
				default:
				}
				continue
			}
			s.deliver(signalResponse{message: message, err: signalMessageError(message, s.room)})
		case signalproto.TypeIncoming:
			if signalproto.ValidateEndpoint(message.Endpoint) != nil {
				continue
			}
			select {
			case s.incomingCh <- incoming{message: message}:
			case <-done:
				return
			}
		}
	}
}

// isListenRejection reports whether an error message is the server's
// answer to a LISTEN: such errors carry no session and one of the codes the
// server uses to refuse a registration.
func isListenRejection(message signalproto.Message) bool {
	if message.Session != "" {
		return false
	}
	switch message.Code {
	case "rate_limited", "too_many_rooms", "invalid_room":
		return true
	}
	return false
}

// drainListenErrors drops LISTEN rejections left over from an earlier
// registration, so they are not attributed to the next one.
func (s *signalControl) drainListenErrors() {
	for {
		select {
		case <-s.listenErrCh:
		default:
			return
		}
	}
}

// signalMessageError maps a signaling error code to a sentinel error.
//
// Every branch wraps with %w, keeping errors.Is working while adding the room
// to the message: one process may serve several rooms, and "no listener in
// room" without the room name is impossible to diagnose.
func signalMessageError(message signalproto.Message, room string) error {
	switch message.Code {
	case "no_listener":
		return fmt.Errorf("%w (room=%s)", ErrNoListener, room)
	case "strategy_mismatch":
		return fmt.Errorf("%w (room=%s)", ErrStrategyMismatch, room)
	case "room_busy", "rate_limited":
		return fmt.Errorf("%w (room=%s, code=%s)", ErrResourceLimit, room, message.Code)
	case "unsupported_nat_pair":
		return fmt.Errorf("%w (room=%s)", ErrUnsupportedNATPair, room)
	case "punch_failed":
		return fmt.Errorf("%w (room=%s)", ErrPunchFailed, room)
	default:
		if message.Error != "" {
			return fmt.Errorf("burrow: signaling error (room=%s): %s", room, message.Error)
		}
		return fmt.Errorf("burrow: signaling error (room=%s, code=%s)", room, message.Code)
	}
}

func (s *signalControl) deliver(response signalResponse) {
	s.mu.Lock()
	channel := s.pending[response.message.Session]
	s.mu.Unlock()
	if channel != nil {
		select {
		case channel <- response:
		default:
		}
	}
}

func (s *signalControl) invalidate(conn *websocket.Conn, done chan struct{}) {
	s.mu.Lock()
	if s.conn != conn || s.connDone != done {
		s.mu.Unlock()
		return
	}
	s.conn = nil
	s.connDone = nil
	close(done)
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		_ = conn.Close(websocket.StatusGoingAway, "signal connection lost")
	}
}

func (s *signalControl) reset() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.generation++
	if s.conn == nil {
		s.mu.Unlock()
		return
	}
	conn, done := s.conn, s.connDone
	s.conn, s.connDone = nil, nil
	if done != nil {
		close(done)
	}
	s.mu.Unlock()
	_ = conn.Close(websocket.StatusGoingAway, "network changed")
}

func (s *signalControl) write(ctx context.Context, message signalproto.Message) error {
	conn, _, err := s.ensureConnected(ctx)
	if err != nil {
		return err
	}
	data, err := signalproto.Encode(message)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		s.mu.Lock()
		done := s.connDone
		s.mu.Unlock()
		if done != nil {
			s.invalidate(conn, done)
		}
		return err
	}
	return nil
}

func (s *signalControl) dial(ctx context.Context, request signalproto.Message) (signalproto.Message, error) {
	responseCh := make(chan signalResponse, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return signalproto.Message{}, errSignalClosed
	}
	s.pending[request.Session] = responseCh
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, request.Session)
		s.mu.Unlock()
	}()

	request.Type = signalproto.TypeDial
	resend := time.NewTicker(dialResendInterval)
	defer resend.Stop()
	// lastErr is the most recent failure to reach the signal server. If the
	// context expires while retrying, it is reported along with the context
	// error, so a refusal such as HTTP 429 is not hidden behind a bare
	// "context deadline exceeded".
	var lastErr error
	expired := func() error {
		if lastErr != nil {
			return fmt.Errorf("%w (last signal error: %w)", ctx.Err(), lastErr)
		}
		return ctx.Err()
	}
	for {
		_, done, err := s.ensureConnected(ctx)
		if err != nil {
			if errors.Is(err, errSignalClosed) {
				return signalproto.Message{}, err
			}
			if ctx.Err() != nil {
				if !errors.Is(err, ctx.Err()) {
					lastErr = err
				}
				return signalproto.Message{}, expired()
			}
			lastErr = err
			select {
			case <-time.After(200 * time.Millisecond):
				continue
			case <-ctx.Done():
				return signalproto.Message{}, expired()
			}
		}
		if err := s.write(ctx, request); err != nil {
			if ctx.Err() != nil {
				return signalproto.Message{}, expired()
			}
			lastErr = err
			continue
		}
		lastErr = nil
		select {
		case response := <-responseCh:
			if response.err != nil {
				return signalproto.Message{}, response.err
			}
			return response.message, nil
		case <-done:
			continue
		case <-resend.C:
			continue
		case <-ctx.Done():
			return signalproto.Message{}, fmt.Errorf("waiting for pairing canceled: %w", ctx.Err())
		}
	}
}

func (s *signalControl) ready(ctx context.Context, message signalproto.Message) error {
	message.Type = signalproto.TypeReady
	return s.write(ctx, message)
}

func (s *signalControl) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conn, done := s.conn, s.connDone
	s.conn, s.connDone = nil, nil
	if done != nil {
		close(done)
	}
	for _, channel := range s.pending {
		select {
		case channel <- signalResponse{err: errSignalClosed}:
		default:
		}
	}
	s.mu.Unlock()
	if conn != nil {
		return conn.Close(websocket.StatusNormalClosure, "closed")
	}
	return nil
}

type listenerCtrl struct {
	ctrl       *signalControl
	room       string
	incomingCh chan incoming
	debugLog   DebugLogFunc
	infoLog    LogFunc

	// transport and capacity are used only by the UDP packet API: LISTEN must
	// carry Transport=udp to match the server registry's (transport, room)
	// key, and capacity declares the room capacity. The TCP path leaves both
	// zero; the server normalizes an empty Transport to tcp.
	transport string
	capacity  int

	mu   sync.Mutex
	seen map[string]*listenerSessionState

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type listenerSessionState struct {
	seenAt time.Time
	ready  *signalproto.Message
}

func newListenerCtrl(ctx context.Context, cfg Config) (*listenerCtrl, error) {
	return newListenerCtrlWith(ctx, cfg.SignalAddr, cfg.Room, cfg.Mode, cfg.Control, cfg.ResolveIP,
		cfg.AcceptQueue, cfg.DebugLog, cfg.InfoLog, "", 0, cfg.tcpNetwork())
}

// newListenerCtrlWith is the parameterized form of newListenerCtrl. It does
// not depend on the TCP Config, so the UDP packet API (packet_listener.go)
// reuses the same signaling logic (WebSocket management, deduplication,
// renewal). With an empty transport, LISTEN carries no Transport/Capacity,
// exactly as on the TCP path.
func newListenerCtrlWith(ctx context.Context, signalAddr, room, mode string, control func(network, address string, c syscall.RawConn) error, resolveIP ResolveIPFunc, queue int, debugLog DebugLogFunc, infoLog LogFunc, transport string, capacity int, tcpNetwork string) (*listenerCtrl, error) {
	ctx, cancel := context.WithCancel(ctx)
	ctrl := newSignalControl(signalAddr, room, mode, control, resolveIP, queue, tcpNetwork)
	listener := &listenerCtrl{
		ctrl:       ctrl,
		room:       room,
		incomingCh: ctrl.incomingCh,
		debugLog:   debugLog,
		infoLog:    infoLog,
		transport:  transport,
		capacity:   capacity,
		seen:       make(map[string]*listenerSessionState),
		cancel:     cancel,
	}
	listener.wg.Go(func() { listener.reconnectLoop(ctx) })
	return listener, nil
}

// listenMessage builds the LISTEN message used to register and renew.
func (c *listenerCtrl) listenMessage() signalproto.Message {
	return signalproto.Message{
		Type:      signalproto.TypeListen,
		Room:      c.room,
		Transport: c.transport,
		Capacity:  c.capacity,
	}
}

func (c *listenerCtrl) reconnectLoop(ctx context.Context) {
	ticker := time.NewTicker(listenRenewInterval)
	defer ticker.Stop()
	// failures counts consecutive failures (connection errors and LISTEN
	// rejections) and resets once the room is joined. While registration
	// fails this loop keeps retrying, so it backs off with
	// listenRetryBackoff and logs only the first failure and every
	// listenRetryLogEvery-th one: problems show up immediately without
	// flooding the log.
	var failures int
	// joined is true while the server holds our registration, so "joined"
	// is logged once per (re)join rather than on every renewal.
	joined := false
	fail := func(err error) bool {
		joined = false
		failures++
		if failures == 1 || failures%listenRetryLogEvery == 0 {
			infoLogf(c.infoLog, "burrow: listener cannot join room on signal server (room=%s, attempt %d): %v",
				c.room, failures, err)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(listenRetryBackoff(failures)):
			return true
		}
	}
	for {
		_, done, err := c.ctrl.ensureConnected(ctx)
		if err == nil {
			c.ctrl.drainListenErrors()
			err = c.ctrl.write(ctx, c.listenMessage())
		}
		// The server does not acknowledge LISTEN, only rejects it. Before
		// reporting a join, give a rejection a moment to arrive.
		if err == nil && !joined {
			err = c.awaitListenRejection(ctx, done)
		}
		if err != nil {
			if ctx.Err() != nil || !fail(err) {
				return
			}
			continue
		}
		if !joined {
			// Restart the renewal period from this join, so ticks that piled
			// up during a backoff do not trigger an immediate extra renewal.
			ticker.Reset(listenRenewInterval)
			if failures > 0 {
				infoLogf(c.infoLog, "burrow: listener reconnected to signal server and joined room (room=%s, after %d failures)",
					c.room, failures)
			} else {
				debugLogf(c.debugLog, "burrow: listener joined room (room=%s, signal=%s)",
					c.room, c.ctrl.addr)
			}
		} else {
			debugLogf(c.debugLog, "burrow: listener renewed room (room=%s)", c.room)
		}
		joined = true
		failures = 0
		select {
		case <-ctx.Done():
			return
		case <-done:
			joined = false
			debugLogf(c.debugLog, "burrow: listener signaling connection lost, reconnecting (room=%s)", c.room)
		case message := <-c.ctrl.listenErrCh:
			if !fail(listenRejectionError(message, c.room)) {
				return
			}
		case <-ticker.C:
			// Loop around to renew.
		}
	}
}

// listenRejectWait is how long the listener waits for a LISTEN rejection
// before it reports having joined the room.
const listenRejectWait = 500 * time.Millisecond

// awaitListenRejection waits up to listenRejectWait for the server to
// reject the LISTEN just sent. It returns nil when no rejection arrives.
func (c *listenerCtrl) awaitListenRejection(ctx context.Context, done <-chan struct{}) error {
	timer := time.NewTimer(listenRejectWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return errors.New("signal connection lost")
	case message := <-c.ctrl.listenErrCh:
		return listenRejectionError(message, c.room)
	case <-timer.C:
		return nil
	}
}

func listenRejectionError(message signalproto.Message, room string) error {
	return fmt.Errorf("LISTEN rejected: %w", signalMessageError(message, room))
}

// listenRetryBackoff doubles the reconnect delay from 1s up to
// listenRetryMaxBackoff, avoiding a futile reconnect every second during a
// long signaling outage.
func listenRetryBackoff(failures int) time.Duration {
	backoff := time.Second << min(failures-1, 5)
	return min(backoff, listenRetryMaxBackoff)
}

func (c *listenerCtrl) markSeen(session string) (bool, *signalproto.Message) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if state, exists := c.seen[session]; exists {
		if state.ready == nil {
			return true, nil
		}
		ready := *state.ready
		return true, &ready
	}
	for known, state := range c.seen {
		if now.Sub(state.seenAt) > sessionRetention {
			delete(c.seen, known)
		}
	}
	c.seen[session] = &listenerSessionState{seenAt: now}
	return false, nil
}

func (c *listenerCtrl) rememberReady(session string, message signalproto.Message) {
	c.mu.Lock()
	state := c.seen[session]
	if state == nil {
		state = &listenerSessionState{seenAt: time.Now()}
		c.seen[session] = state
	}
	copy := message
	state.ready = &copy
	c.mu.Unlock()
}

func (c *listenerCtrl) forget(session string) {
	c.mu.Lock()
	delete(c.seen, session)
	c.mu.Unlock()
}

func (c *listenerCtrl) ready(ctx context.Context, message signalproto.Message) error {
	message.Room = c.room
	return c.ctrl.ready(ctx, message)
}

func (c *listenerCtrl) reject(ctx context.Context, session, code, message string) error {
	return c.ctrl.write(ctx, signalproto.Message{
		Type:      signalproto.TypeError,
		Room:      c.room,
		Session:   session,
		Code:      code,
		Error:     message,
		Transport: c.transport,
	})
}

func (c *listenerCtrl) Close() error {
	c.cancel()
	_ = c.ctrl.close()
	c.wg.Wait()
	return nil
}

func (c *listenerCtrl) reset() {
	c.ctrl.reset()
}

// signalParams merges command and optional query values into the full set
// of signaling parameters. The result can go into the URL query or, base64
// encoded, into a Cookie.
func signalParams(command string, queryValues ...url.Values) url.Values {
	query := url.Values{}
	if len(queryValues) > 0 && queryValues[0] != nil {
		maps.Copy(query, queryValues[0])
	}
	query.Set(signalproto.CommandKey, command)
	return query
}

// signalURL builds the WebSocket URL from a signal address and parameters.
//
// The address path is kept as a fixed prefix. A non-empty query is encoded
// into the URL:
//
//	ws://host/abc + {command:echo} -> ws://host/abc?command=echo
//
// An empty query keeps only the prefix (in header mode the parameters go in
// a Cookie instead):
//
//	ws://host/abc -> ws://host/abc
func signalURL(addr string, query url.Values) (string, error) {
	if !strings.Contains(addr, "://") {
		addr = "ws://" + addr
	}
	parsed, err := url.Parse(addr)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid signal address %q", addr)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return "", fmt.Errorf("signal address must use ws or wss: %q", addr)
	}
	parsed.Path = normalizeSignalPrefix(parsed.Path)
	if len(query) > 0 {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

// normalizeSignalPrefix normalizes the path prefix the same way as the
// server's normalizePrefix.
func normalizeSignalPrefix(prefix string) string {
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return "/"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return prefix
}

func signalHostPort(addr string) (string, error) {
	if !strings.Contains(addr, "://") {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return "", fmt.Errorf("invalid signal address %q: %w", addr, err)
		}
		return addr, nil
	}
	parsed, err := url.Parse(addr)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid signal address %q", addr)
	}
	if parsed.Port() != "" {
		return parsed.Host, nil
	}
	switch parsed.Scheme {
	case "ws", "http":
		return net.JoinHostPort(parsed.Hostname(), "80"), nil
	case "wss", "https":
		return net.JoinHostPort(parsed.Hostname(), "443"), nil
	default:
		return "", fmt.Errorf("signal address has no port and an unknown scheme: %q", addr)
	}
}

func dialSignalTCP(ctx context.Context, dialer *net.Dialer, address string, resolveIP ResolveIPFunc, tcpNetwork string) (net.Conn, error) {
	if tcpNetwork == "" {
		tcpNetwork = "tcp4"
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil || resolveIP == nil {
		return dialer.DialContext(ctx, tcpNetwork, address)
	}
	addresses, err := resolveIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve signal server %s: %w", host, err)
	}
	var dialErrors []error
	for _, ip := range addresses {
		ip = ip.Unmap()
		if (tcpNetwork == "tcp4") != ip.Is4() {
			continue
		}
		target := net.JoinHostPort(ip.String(), port)
		conn, dialErr := dialer.DialContext(ctx, tcpNetwork, target)
		if dialErr == nil {
			return conn, nil
		}
		dialErrors = append(dialErrors, fmt.Errorf("%s: %w", target, dialErr))
	}
	if len(dialErrors) == 0 {
		return nil, fmt.Errorf("signal server %s has no usable %s address", host, tcpNetwork)
	}
	return nil, fmt.Errorf("connect to signal server %s: %w", host, errors.Join(dialErrors...))
}

func dialSignalWebSocket(ctx context.Context, addr, command string, query url.Values, mode string, control func(network, address string, c syscall.RawConn) error, resolveIP ResolveIPFunc, localAddr, tcpNetwork string) (*websocket.Conn, error) {
	params := signalParams(command, query)
	// mode == "header": parameters go base64-encoded into a Cookie header
	// instead of the URL. Any other value, including empty or unknown, puts
	// them in the URL query.
	hidden := mode == signalModeHeader
	urlParams := params
	if hidden {
		urlParams = nil
	}
	wsAddr, err := signalURL(addr, urlParams)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Control: socketControl(control)}
	if localAddr != "" {
		dialer.LocalAddr = mustTCPAddr(localAddr)
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialSignalTCP(ctx, dialer, address, resolveIP, tcpNetwork)
		},
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          0,
		MaxIdleConnsPerHost:   0,
		IdleConnTimeout:       0,
		TLSHandshakeTimeout:   defaultSignalTimeout,
		ResponseHeaderTimeout: defaultSignalTimeout,
	}
	client := &http.Client{Transport: transport}
	header := http.Header{}
	header.Set("User-Agent", signalUserAgent)
	if hidden {
		// Header mode: parameters go base64-encoded into a Cookie rather
		// than appearing as plain text in the URL.
		header.Set("Cookie", signalproto.HideCookieName+"="+signalproto.EncodeHiddenQuery(params))
	}
	// Strip the port from the Host header, which would otherwise carry the
	// URL's host:port.
	options := &websocket.DialOptions{HTTPClient: client, HTTPHeader: header}
	if parsed, parseErr := url.Parse(wsAddr); parseErr == nil {
		options.Host = parsed.Hostname()
	}
	conn, _, err := websocket.Dial(ctx, wsAddr, options)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return conn, nil
}
