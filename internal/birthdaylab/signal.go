package birthdaylab

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/signalproto"
)

var ErrNoListener = errors.New("birthday prototype: no server is listening in the room")

type ListenerSignal struct {
	room      string
	conn      *websocket.Conn
	transport *http.Transport
	ctx       context.Context
	cancel    context.CancelFunc
	writeMu   sync.Mutex
	stateMu   sync.Mutex
	pending   map[string]signalproto.Message
	wg        sync.WaitGroup
}

type IncomingRequest struct {
	Version       int
	Session       string
	ClientAddress string
	Parameters    signalproto.StrategyParameters
}

func OpenListenerSignal(parent context.Context, signalAddress, room, bindIP string) (*ListenerSignal, error) {
	wsURL, err := websocketURL(signalAddress, "/signal", 0)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	conn, transport, err := dialWebSocket(ctx, wsURL, net.JoinHostPort(bindIP, "0"))
	if err != nil {
		cancel()
		return nil, err
	}
	signal := &ListenerSignal{
		room:      room,
		conn:      conn,
		transport: transport,
		ctx:       ctx,
		cancel:    cancel,
		pending:   make(map[string]signalproto.Message),
	}
	if err := signal.send(signalproto.Message{
		Type: signalproto.TypeListen, Room: room, Transport: signalproto.TransportTCP, Capacity: 1,
	}); err != nil {
		signal.Close()
		return nil, err
	}
	signal.wg.Add(1)
	go signal.renewLoop()
	return signal, nil
}

func (s *ListenerSignal) WaitIncoming(ctx context.Context) (IncomingRequest, error) {
	for {
		messageType, data, err := s.conn.Read(ctx)
		if err != nil {
			return IncomingRequest{}, err
		}
		if messageType != websocket.MessageText {
			continue
		}
		message, err := signalproto.Decode(data)
		if err != nil || message.Type != signalproto.TypeIncoming || signalproto.ValidateEndpoint(message.Endpoint) != nil {
			continue
		}
		s.stateMu.Lock()
		s.pending[message.Session] = message
		s.stateMu.Unlock()
		return IncomingRequest{
			Version:       message.Version,
			Session:       message.Session,
			ClientAddress: message.Endpoint,
			Parameters:    message.Parameters,
		}, nil
	}
}

func (s *ListenerSignal) Ready(session, publicAddress string) error {
	s.stateMu.Lock()
	request, exists := s.pending[session]
	s.stateMu.Unlock()
	if !exists {
		return fmt.Errorf("birthday session %s missing", session)
	}
	return s.send(signalproto.Message{
		Type:         signalproto.TypeReady,
		Room:         s.room,
		Session:      session,
		Endpoint:     publicAddress,
		Strategy:     signalproto.StrategyBirthday,
		Parameters:   request.Parameters,
		Fingerprint:  request.Fingerprint,
		SelectionKey: request.SelectionKey,
		Transport:    signalproto.TransportTCP,
	})
}

func (s *ListenerSignal) Reject(session, code string, err error) error {
	return s.send(signalproto.Message{
		Type:      signalproto.TypeError,
		Room:      s.room,
		Session:   session,
		Code:      code,
		Error:     err.Error(),
		Transport: signalproto.TransportTCP,
	})
}

func (s *ListenerSignal) Close() {
	s.cancel()
	_ = s.conn.Close(websocket.StatusNormalClosure, "experiment complete")
	s.transport.CloseIdleConnections()
	s.wg.Wait()
}

func (s *ListenerSignal) renewLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			_ = s.send(signalproto.Message{
				Type: signalproto.TypeListen, Room: s.room, Transport: signalproto.TransportTCP, Capacity: 1,
			})
		}
	}
}

func (s *ListenerSignal) send(message signalproto.Message) error {
	data, err := signalproto.Encode(message)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, websocketTimeout)
	defer cancel()
	return s.conn.Write(ctx, websocket.MessageText, data)
}

func PairClient(
	ctx context.Context,
	signalAddress string,
	room string,
	bindIP string,
	session string,
	parameters signalproto.StrategyParameters,
) (string, error) {
	probeURL, err := websocketURL(signalAddress, "/nat", 0)
	if err != nil {
		return "", err
	}
	parsedProbeURL, err := url.Parse(probeURL)
	if err != nil {
		return "", err
	}
	query := parsedProbeURL.Query()
	query.Set("room", room)
	query.Set("session", session)
	parsedProbeURL.RawQuery = query.Encode()
	probe, probeTransport, err := dialWebSocket(ctx, parsedProbeURL.String(), net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return "", err
	}
	defer probe.Close(websocket.StatusNormalClosure, "")
	defer probeTransport.CloseIdleConnections()
	probeType, probeData, err := probe.Read(ctx)
	if err != nil {
		return "", fmt.Errorf("read signal probe: %w", err)
	}
	if probeType != websocket.MessageText {
		return "", fmt.Errorf("unexpected signal probe message type: %v", probeType)
	}
	var observation signalproto.ProbeResponse
	if err := json.Unmarshal(probeData, &observation); err != nil {
		return "", err
	}

	wsURL, err := websocketURL(signalAddress, "/signal", 0)
	if err != nil {
		return "", err
	}
	conn, transport, err := dialWebSocket(ctx, wsURL, net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return "", err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	defer transport.CloseIdleConnections()
	request := signalproto.Message{
		Type:        signalproto.TypeDial,
		Room:        room,
		Session:     session,
		Endpoint:    observation.Endpoint,
		Token:       observation.Token,
		Strategy:    signalproto.StrategyBirthday,
		Parameters:  parameters,
		Fingerprint: parameters.Fingerprint(),
		Transport:   signalproto.TransportTCP,
	}
	data, err := signalproto.Encode(request)
	if err != nil {
		return "", err
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return "", err
	}
	for {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			return "", err
		}
		if messageType != websocket.MessageText {
			continue
		}
		message, err := signalproto.Decode(data)
		if err != nil || message.Session != session {
			continue
		}
		switch message.Type {
		case signalproto.TypePeer:
			if signalproto.ValidateEndpoint(message.Endpoint) != nil {
				continue
			}
			return message.Endpoint, nil
		case signalproto.TypeError:
			if message.Code == "no_listener" {
				return "", ErrNoListener
			}
			return "", fmt.Errorf("Signal %s: %s", message.Code, message.Error)
		}
	}
}

func NewSessionID() (string, error) {
	var value [12]byte
	if _, err := cryptorand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
