package burrow

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

// This file holds the configuration of the UDP packet API.
//
// The packet API currently supports only the direct and predictive
// strategies. Birthday and auto are not supported over UDP yet; the related
// fields stay in PacketConfig so the signature is stable, but normalize
// rejects any Strategy other than direct or predictive.

// UDP resource limits.
const (
	// predictiveUDPMaxPortRange caps the predictive UDP port scan window.
	predictiveUDPMaxPortRange = 4096

	// candidatePacketRateLimit and candidatePacketBurst rate limit
	// candidate packets: 4096 packets/s with a burst of 1024.
	candidatePacketRateLimit = 4096
	candidatePacketBurst     = 1024

	defaultUDPPairTimeout        = 60 * time.Second
	defaultUDPPunchTimeout       = 20 * time.Second
	defaultUDPPredictiveTimeout  = 3 * time.Second
	defaultUDPPredictiveConc     = 64
	defaultUDPMaxConcurrentTries = 2

	// defaultSTUNTimeout bounds a single STUN query. It is converted with
	// stun.WithRTO into the total duration of 7 linear retransmissions.
	defaultSTUNTimeout   = 5 * time.Second
	defaultSTUNCacheTTL  = 10 * time.Minute
	defaultInitialSample = 3
	defaultMaxSample     = 8
)

// STUNOptions configures the STUN servers used to collect the UDP NAT
// profile and to query address mappings.
type STUNOptions struct {
	// Servers lists STUN servers such as "stun.example.com:3478". The
	// explicit strategies (direct, predictive) need at least one. Auto will
	// need at least three covering two distinct public IPs once it is
	// supported by the packet API.
	Servers []string

	// InitialSamples and MaxSamples bound how many samples
	// collectUDPProfileSample takes while trying to converge on a definite
	// NAT class.
	InitialSamples int
	MaxSamples     int

	// Timeout bounds a single STUN Binding query.
	Timeout time.Duration

	// CacheTTL is how long a cached UDP NAT profile stays valid. The
	// default is 10 minutes.
	CacheTTL time.Duration
}

func (o STUNOptions) normalize() (STUNOptions, error) {
	if o.InitialSamples <= 0 {
		o.InitialSamples = defaultInitialSample
	}
	if o.MaxSamples <= 0 {
		o.MaxSamples = defaultMaxSample
	}
	if o.MaxSamples < o.InitialSamples {
		return o, errors.New("burrow: STUNOptions.MaxSamples must not be less than InitialSamples")
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultSTUNTimeout
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = defaultSTUNCacheTTL
	}
	return o, nil
}

// PacketConfig configures UDP hole punching. Shared fields have the same
// meaning as in the TCP Config (see burrow.go); it is a separate type so it
// can carry the UDP-specific STUN, predictive and proof settings.
type PacketConfig struct {
	SignalAddr    string
	Mode          string
	Room          string
	ResolveIP     ResolveIPFunc
	BindInterface string
	InterfaceName func() string
	InterfaceAddr func(string) string
	Control       func(network, address string, c syscall.RawConn) error
	DebugLog      DebugLogFunc
	InfoLog       LogFunc

	STUN                  STUNOptions
	PairTimeout           time.Duration
	PunchTimeout          time.Duration
	Strategy              Strategy
	StableSide            bool
	Predictive            PredictiveOptions
	Birthday              BirthdayOptions
	MaxConcurrentAttempts int

	// SignProof and VerifyProof are the session proof hooks.
	//
	// The listener calls VerifyProof on the DIAL message before allocating
	// candidate sockets; the dialer calls it on the READY/PEER message
	// before sending candidate packets. Each side calls SignProof to sign
	// its own DIAL/READY message before sending it. Both hooks are
	// optional: when nil, no proof is signed or checked, which keeps hole
	// punching usable before the caller wires up its own key derivation.
	SignProof   ProofSigner
	VerifyProof ProofVerifier
}

func (c PacketConfig) normalize() (PacketConfig, error) {
	if c.SignalAddr == "" {
		return c, errors.New("burrow: PacketConfig.SignalAddr is required")
	}
	if c.Room == "" {
		return c, errors.New("burrow: PacketConfig.Room is required")
	}
	if err := signalproto.ValidateRoom(c.Room); err != nil {
		return c, fmt.Errorf("burrow: invalid PacketConfig.Room: %w", err)
	}
	if len(c.STUN.Servers) == 0 {
		return c, errors.New("burrow: PacketConfig.STUN.Servers is required")
	}
	stun, err := c.STUN.normalize()
	if err != nil {
		return c, err
	}
	c.STUN = stun

	if c.PairTimeout <= 0 {
		c.PairTimeout = defaultUDPPairTimeout
	}
	if c.PunchTimeout <= 0 {
		c.PunchTimeout = defaultUDPPunchTimeout
	}

	switch c.Strategy {
	case StrategyDirect, StrategyPredictive:
	case "":
		return c, errors.New("burrow: PacketConfig.Strategy is required (direct or predictive)")
	case StrategyBirthday, StrategyAuto:
		return c, fmt.Errorf("burrow: strategy %q is not supported by the packet API yet", c.Strategy)
	default:
		return c, fmt.Errorf("burrow: unknown strategy %q", c.Strategy)
	}

	if c.Predictive.PortScanRange <= 0 {
		c.Predictive.PortScanRange = 512
	}
	if c.Predictive.PortScanRange > predictiveUDPMaxPortRange {
		return c, fmt.Errorf("burrow: predictive UDP port range exceeds limit %d: %d",
			predictiveUDPMaxPortRange, c.Predictive.PortScanRange)
	}
	if c.Predictive.Concurrency <= 0 {
		c.Predictive.Concurrency = defaultUDPPredictiveConc
	}
	if c.Predictive.Concurrency > candidateSocketBudget-1 {
		return c, fmt.Errorf("burrow: predictive concurrency exceeds limit: %d", c.Predictive.Concurrency)
	}
	if c.Predictive.AttemptTimeout <= 0 {
		c.Predictive.AttemptTimeout = defaultUDPPredictiveTimeout
	}

	// Predictive: the stable side scans a port window centered on the
	// peer's mapping from one socket, while the changing side aims one
	// socket at the stable endpoint. As on the TCP path, the listener is
	// always the stable side, so StableSide is not enforced here; the roles
	// are assigned during establishment.

	if c.MaxConcurrentAttempts <= 0 {
		c.MaxConcurrentAttempts = defaultUDPMaxConcurrentTries
	}
	if c.MaxConcurrentAttempts > 16 {
		return c, fmt.Errorf("burrow: max concurrent attempts exceeds limit: %d", c.MaxConcurrentAttempts)
	}

	return c, nil
}

func (c PacketConfig) interfaceName() string {
	if c.BindInterface != "" {
		return c.BindInterface
	}
	if c.InterfaceName != nil {
		return c.InterfaceName()
	}
	return ""
}

func (c PacketConfig) strategyParameters() signalproto.StrategyParameters {
	// StrategyParameters carries only PredictiveRange; PredictiveConcurrency
	// was removed in protocol v5 to keep the fingerprint byte-for-byte
	// compatible. Concurrency is a local setting that both sides do not
	// negotiate, so it does not belong in the fingerprint.
	return signalproto.StrategyParameters{
		PredictiveRange: c.Predictive.PortScanRange,
	}
}
