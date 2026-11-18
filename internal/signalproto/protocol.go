// Package signalproto defines the wire protocol spoken between burrow peers and
// the burrow-signal server.
package signalproto

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// Version is the protocol version carried by every WebSocket signal
	// message and by ProbeResponse.
	Version = 1
	// MaxMessageBytes is the largest signal message accepted by Decode.
	MaxMessageBytes = 4096
)

// Transport values for Message.Transport. A message that omits the field is
// treated as TransportTCP; Decode fills it in.
const (
	TransportTCP = "tcp"
	TransportUDP = "udp"
)

// Every signaling entry point is "<prefix>?command=<name>". The prefix is chosen
// by the operator (for example /signal) and the command query parameter selects
// the function, so the server can sit behind an arbitrary path.
const (
	CommandKey = "command"

	CommandSignal = "signal"
	CommandEcho   = "echo"
	CommandNAT    = "nat"
	CommandHealth = "health"
)

// HideCookieName is the cookie that carries the signaling query parameters in
// hidden mode. The name is deliberately short and unremarkable so that an
// on-path observer cannot easily tell the traffic is signaling.
const HideCookieName = "br"

// EncodeHiddenQuery encodes query parameters into a string usable as a cookie
// value. It uses unpadded base64url (A-Z a-z 0-9 - _), which contains none of
// the characters that are illegal in a cookie value (`;`, space, `,`, `=`).
func EncodeHiddenQuery(values url.Values) string {
	return base64.RawURLEncoding.EncodeToString([]byte(values.Encode()))
}

// DecodeHiddenQuery reverses EncodeHiddenQuery. An empty string decodes to
// empty Values.
func DecodeHiddenQuery(raw string) (url.Values, error) {
	if raw == "" {
		return url.Values{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(decoded))
}

type MessageType string

const (
	TypeListen   MessageType = "listen"
	TypeDial     MessageType = "dial"
	TypeIncoming MessageType = "incoming"
	TypeReady    MessageType = "ready"
	TypePeer     MessageType = "peer"
	TypeError    MessageType = "error"
)

type Strategy string

const (
	StrategyAuto       Strategy = "auto"
	StrategyDirect     Strategy = "direct"
	StrategyPredictive Strategy = "predictive"
	StrategyBirthday   Strategy = "birthday"
)

type Selection string

const (
	SelectionACK    Selection = "ack"
	SelectionFanout Selection = "fanout"
)

type NATClass string

const (
	NATStable     NATClass = "stable"
	NATSequential NATClass = "sequential"
	NATDispersed  NATClass = "dispersed"
	NATUnknown    NATClass = "unknown"
)

type NATProfile struct {
	Class            NATClass `json:"class"`
	PublicIP         string   `json:"public_ip,omitempty"`
	Samples          int      `json:"samples"`
	PredictionHits   int      `json:"prediction_hits,omitempty"`
	PredictionTrials int      `json:"prediction_trials,omitempty"`
	VerifiedCrossIP  bool     `json:"verified_cross_ip,omitempty"`
}

// StrategyParameters are the punching parameters chosen by the Dialer and
// adopted by the Listener.
//
// Only settings that must match on both sides for punching to work belong
// here: the birthday-collision probability model depends on both sides using
// the same candidate count and port range, and a selection mismatch breaks the
// handshake outright. Purely local resource settings (such as predictive
// concurrency) are excluded: they only decide how many sockets this side opens,
// differing values do not affect correctness, and including them in the
// fingerprint would add false constraints.
type StrategyParameters struct {
	PredictiveRange    int           `json:"predictive_range,omitempty"`
	BirthdayCandidates int           `json:"birthday_candidates,omitempty"`
	BirthdayMinPort    uint16        `json:"birthday_min_port,omitempty"`
	BirthdayMaxPort    uint16        `json:"birthday_max_port,omitempty"`
	BirthdayWarmup     time.Duration `json:"birthday_warmup,omitempty"`
	BirthdayTimeout    time.Duration `json:"birthday_timeout,omitempty"`
	BirthdayGrace      time.Duration `json:"birthday_grace,omitempty"`
	BirthdayMaxSuccess int           `json:"birthday_max_success,omitempty"`
	BirthdaySelection  Selection     `json:"birthday_selection,omitempty"`
}

// Fingerprint returns a short hash of the parameters. It is the TCP
// fingerprint: FingerprintForTransport(TransportTCP) returns the same value.
func (p StrategyParameters) Fingerprint() string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// FingerprintForTransport returns the strategy fingerprint bound to a
// transport. For TransportTCP (or an empty transport, which means tcp) it
// equals Fingerprint(); any other transport changes the output, so peers using
// different transports never agree on a fingerprint.
func (p StrategyParameters) FingerprintForTransport(transport string) string {
	if transport == "" || transport == TransportTCP {
		return p.Fingerprint()
	}
	b, _ := json.Marshal(p)
	b = append(b, '|')
	b = append(b, transport...)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type Message struct {
	Version      int                `json:"version"`
	Type         MessageType        `json:"type"`
	Room         string             `json:"room,omitempty"`
	Session      string             `json:"session,omitempty"`
	Endpoint     string             `json:"endpoint,omitempty"`
	Token        string             `json:"token,omitempty"`
	Profile      *NATProfile        `json:"profile,omitempty"`
	Strategy     Strategy           `json:"strategy,omitempty"`
	Parameters   StrategyParameters `json:"parameters,omitempty"`
	Fingerprint  string             `json:"fingerprint,omitempty"`
	SelectionKey string             `json:"selection_key,omitempty"`
	Code         string             `json:"code,omitempty"`
	Error        string             `json:"error,omitempty"`

	// Transport is the transport this message is about: tcp or udp. It may
	// be omitted; Decode normalizes an empty value to TransportTCP.
	Transport string `json:"transport,omitempty"`
	// Proof authenticates a signal message between the two peers. It is
	// required for udp, where there is no probe token.
	Proof string `json:"proof,omitempty"`
	// Capacity is the room capacity announced by a LISTEN message. When it
	// is omitted the server uses its default.
	Capacity int `json:"capacity,omitempty"`
}

type ProbeResponse struct {
	Version   int    `json:"version"`
	Endpoint  string `json:"endpoint"`
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// Encode marshals message with its Version set to Version.
func Encode(message Message) ([]byte, error) {
	message.Version = Version
	return json.Marshal(message)
}

// Decode parses a signal message. It rejects oversized messages, any version
// other than Version and messages without a type, and normalizes an empty
// Transport to TransportTCP.
func Decode(data []byte) (Message, error) {
	if len(data) > MaxMessageBytes {
		return Message{}, errors.New("signal message too large")
	}
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		return Message{}, err
	}
	if message.Version != Version {
		return Message{}, fmt.Errorf("unsupported signal version: %d", message.Version)
	}
	if message.Type == "" {
		return Message{}, errors.New("missing signal message type")
	}
	if message.Transport == "" {
		message.Transport = TransportTCP
	}
	return message, nil
}

// Length limits of the names the signal server accepts.
const (
	MaxRoomLength    = 128
	MaxSessionLength = 64
	MaxCodeLength    = 64
)

// ValidName reports whether value is a name the signal server accepts: not
// empty or blank and at most max bytes long. Rooms, sessions and error codes
// all follow this rule, with the limits above.
func ValidName(value string, max int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= max
}

// ValidateRoom checks a room name against the signal server's rule, so
// clients can reject a bad name when they are configured instead of having
// every LISTEN or DIAL refused at run time.
func ValidateRoom(room string) error {
	if !ValidName(room, MaxRoomLength) {
		return fmt.Errorf("room must be non-blank and at most %d bytes, got %d bytes", MaxRoomLength, len(room))
	}
	return nil
}

func ValidateEndpoint(endpoint string) error {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return err
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("endpoint host is not an IP: %q", host)
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 || value > 65535 {
		return fmt.Errorf("invalid endpoint port: %q", port)
	}
	return nil
}

func ValidateStrategy(strategy Strategy) error {
	switch strategy {
	case StrategyAuto, StrategyDirect, StrategyPredictive, StrategyBirthday:
		return nil
	default:
		return fmt.Errorf("unsupported punch strategy: %q", strategy)
	}
}
