package burrow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

// Errors returned by this package. Test for them with errors.Is.
var (
	// ErrPunchFailed means hole punching did not produce a usable
	// connection, usually because a NAT does not support TCP simultaneous
	// open or the peer did not punch at the same time.
	ErrPunchFailed = errors.New("burrow: hole punching failed")

	// ErrClosed means the Listener or Dialer has been closed.
	//
	// It wraps net.ErrClosed, so both errors.Is(err, ErrClosed) and
	// errors.Is(err, net.ErrClosed) hold. The latter is how standard library
	// consumers such as http.Serve recognize a normal listener shutdown.
	ErrClosed = fmt.Errorf("burrow: closed: %w", net.ErrClosed)

	// ErrNoListener means no Listener is currently registered in the room.
	//
	// It is distinct from "the peer is online but not ready yet": retrying
	// does not help here because the peer is not running at all, so callers
	// can choose to fail fast instead of backing off.
	ErrNoListener = errors.New("burrow: no listener in room")

	// ErrUnsupportedNATPair means the auto strategy could not pick a
	// strategy for this NAT combination.
	ErrUnsupportedNATPair = errors.New("burrow: NAT pair cannot be classified automatically")
	// ErrStrategyMismatch means the two sides disagree on the strategy or
	// its parameters.
	ErrStrategyMismatch = errors.New("burrow: strategy or parameters differ between peers")
	// ErrResourceLimit means the candidate socket budget is exhausted.
	ErrResourceLimit = errors.New("burrow: candidate resource budget exhausted")
	// ErrSelectionFailed means the candidate selection handshake failed.
	ErrSelectionFailed = errors.New("burrow: candidate selection failed")
	// ErrUnsupportedPlatform means the platform lacks the socket options TCP
	// hole punching needs. Burrow relies on SO_REUSEPORT to share one local
	// port between address reflection, listening and dialing. Platforms such
	// as Windows have no equivalent: the package compiles there, but fails
	// explicitly at run time instead of producing unusable connections.
	ErrUnsupportedPlatform = errors.New("burrow: TCP hole punching is not supported on this platform (requires SO_REUSEPORT)")
	errNATProfileStale     = errors.New("burrow: NAT profile public IP changed")
)

// Strategy selects the hole punching strategy. See the package
// documentation for a description of each value.
type Strategy = signalproto.Strategy

const (
	StrategyAuto       = signalproto.StrategyAuto
	StrategyDirect     = signalproto.StrategyDirect
	StrategyPredictive = signalproto.StrategyPredictive
	StrategyBirthday   = signalproto.StrategyBirthday

	AddressFamilyIPv4 = "ipv4"
	AddressFamilyIPv6 = "ipv6"
)

// CandidateSelection selects how the birthday strategy picks a connection
// when several candidates succeed: ack exchanges a selection handshake so both
// sides agree on one connection, fanout delivers every successful candidate.
type CandidateSelection = signalproto.Selection

const (
	CandidateSelectionACK    = signalproto.SelectionACK
	CandidateSelectionFanout = signalproto.SelectionFanout
)

// PredictiveOptions holds the predictive strategy parameters.
//
// PortScanRange is sent by the Dialer to the Listener, so it only takes effect
// on the Dialer; Listen rejects it if set on the Listener.
//
// Concurrency (the number of sockets this side dials in parallel) and
// AttemptTimeout are local settings that are not negotiated, so each side may
// set its own values.
type PredictiveOptions struct {
	PortScanRange  int
	Concurrency    int
	AttemptTimeout time.Duration
}

// BirthdayOptions holds the birthday strategy parameters. All of them are sent
// by the Dialer to the Listener, so they only take effect on the Dialer;
// Listen rejects them if set on the Listener.
type BirthdayOptions struct {
	Selection            CandidateSelection
	Candidates           int
	MinPort              uint16
	MaxPort              uint16
	Warmup               time.Duration
	AttemptTimeout       time.Duration
	SuccessGrace         time.Duration
	MaxSuccessCandidates int
}

// isZero reports whether no negotiated field is set. The Listener uses it to
// reject configuration that would be ignored, since the peer sends these
// values.
func (o BirthdayOptions) isZero() bool {
	return o == BirthdayOptions{}
}

// negotiatedIsZero checks only the negotiated predictive fields.
//
// Concurrency and AttemptTimeout are not signaled: one is the number of
// parallel dial sockets on this side, the other this side's per-round scan
// timeout. Different values on the two sides do not affect correctness, so
// they are local settings and the Listener may set them.
func (o PredictiveOptions) negotiatedIsZero() bool {
	return o.PortScanRange == 0
}

// NATProbeOptions controls how the Dialer probes its NAT for the auto
// strategy. Targets defaults to the signal server.
type NATProbeOptions struct {
	Targets        []string
	InitialSamples int
	MaxSamples     int
	Timeout        time.Duration
	CacheTTL       time.Duration
}

// ResolveIPFunc resolves the signal server host name to IP addresses. Burrow
// still creates and configures the sockets itself, so source port reuse,
// interface binding and hole punching behave the same.
type ResolveIPFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// signalUserAgent is the User-Agent sent in the signaling WebSocket handshake.
// It makes signaling look like an ordinary desktop browser (stable Chrome on
// 64-bit Windows 10) at the HTTP header level instead of Go's default
// "Go-http-client/1.1".
const signalUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Defaults.
const (
	defaultSignalTimeout = 10 * time.Second

	defaultPairTimeout  = 60 * time.Second
	defaultPunchTimeout = 45 * time.Second
	defaultDialTimeout  = 700 * time.Millisecond
	defaultDialInterval = 200 * time.Millisecond

	// tcpKeepAlivePeriod keeps the NAT mapping alive and surfaces silently
	// dropped links quickly. Keepalives are transport-level control segments
	// and never enter the application data stream.
	tcpKeepAlivePeriod = 15 * time.Second

	// Port prediction scan parameters, used only when a scan range is set.
	scanDialTimeout   = 300 * time.Millisecond // per-dial timeout while scanning; short so the window is swept quickly
	scanRoundInterval = 100 * time.Millisecond // pause after each sweep of the window
	scanConcurrency   = 64                     // parallel dials; distinct target ports give distinct 4-tuples

	// Upper bounds of the birthday timing parameters. The Dialer sends these
	// values and the Listener holds Birthday.Candidates armed sockets for
	// Warmup+AttemptTimeout, so without a bound a peer could pin the
	// Listener's sockets and attempt slots indefinitely. The defaults are
	// 250ms, 3s and 250ms.
	maxBirthdayWarmup         = 10 * time.Second
	maxBirthdayAttemptTimeout = 30 * time.Second
	maxBirthdaySuccessGrace   = 5 * time.Second

	// listenRenewInterval is the LISTEN renewal interval. It must be well
	// below the server-side lease.
	listenRenewInterval = 5 * time.Second

	// listenRetryMaxBackoff caps the listener's reconnect backoff.
	listenRetryMaxBackoff = 30 * time.Second

	// listenRetryLogEvery throttles logging of consecutive failures: the first
	// failure is always logged, then one in every listenRetryLogEvery, so a
	// long signal outage does not flood the log.
	listenRetryLogEvery = 10

	// sessionRetention is how long seen sessions are remembered to
	// deduplicate resent INCOMING messages.
	sessionRetention = 2 * time.Minute

	// defaultAcceptQueue is the number of established connections buffered
	// until Accept takes them.
	defaultAcceptQueue = 64

	// listenerCandidateGrace gives the Listener time to receive the candidate
	// from the other direction of the same session.
	listenerCandidateGrace = 5 * time.Second

	// accChCapacity is the buffer for passively accepted connections waiting
	// to be claimed by peer address.
	//
	// A session yields at most two candidates (active and passive), but a
	// scanning peer may retry for several rounds, so a slightly larger buffer
	// keeps the real candidate from being crowded out by unrelated ones.
	accChCapacity = 8
)

// Config configures a Listener or Dialer. SignalAddr and Room are required;
// every other field has a default.
type Config struct {
	// SignalAddr is the signal server address, such as
	// "signal.example.com:9960". The server provides WebSocket signaling and
	// TCP address reflection. Both sides must use the same server.
	SignalAddr string

	// Mode controls how signaling parameters are sent:
	//   ""       (default) in the URL query, e.g. ?command=nat&room=r&session=s.
	//   "header" base64-encoded in a Cookie header instead of the URL, so an
	//            on-path observer does not see command/room/session in clear.
	// The server always accepts both, so any server version works.
	Mode string

	// DebugLog optionally routes diagnostic logs to the caller's logger; the
	// caller handles level filtering. When nil, BURROW_DEBUG controls output
	// through the standard log package.
	DebugLog DebugLogFunc

	// InfoLog optionally receives info-level logs: pairing, successful
	// punches and listener reconnects. When nil, they are dropped.
	InfoLog LogFunc

	// StatsLog optionally receives structured TCP statistics for every
	// established connection once per second. The same numbers are also
	// written to DebugLog as tcp_stats lines; sampling runs only when
	// DebugLog or StatsLog is set.
	//
	// It lets a UI (such as the Android app) consume the numbers directly
	// instead of parsing log lines.
	StatsLog func(TCPStats)

	// ResolveIP optionally takes over resolution of the signal server host
	// name. The host name in the WebSocket URL is kept, so the HTTP Host and
	// TLS SNI still use the name from SignalAddr. Returned addresses matching
	// AddressFamily are tried in order. When nil, the system resolver is used.
	ResolveIP ResolveIPFunc

	// Room is the pairing room name. Both sides must use the same room.
	Room string

	// AddressFamily selects the address family for hole punching: "ipv4" or
	// "ipv6". Empty means ipv4. Address reflection, signaling, listening and
	// dialing all use this family, so one attempt reuses the same local IP and
	// port throughout.
	AddressFamily string

	// BindInterface names the physical interface that punching sockets bind
	// to, such as "en0" or "eth0". When empty the kernel picks the route, but
	// with virtual interfaces such as Docker bridges present it may pick an
	// unroutable source address and punching fails, so setting it is
	// recommended.
	BindInterface string

	// InterfaceName returns the current physical interface name at the start
	// of each attempt, for setups where the default interface changes with
	// the network. BindInterface takes precedence when set.
	InterfaceName func() string

	// InterfaceAddr optionally returns the address of the named interface in
	// the configured AddressFamily. An empty result means the caller cannot
	// provide it. In that case IPv4 tries internal/netiface.IPv4ByName (ioctl
	// on Linux and Android) and then net.InterfaceByName/Addrs; IPv6
	// enumerates global addresses through net.InterfaceByName/Addrs.
	//
	// It avoids the netlink calls behind net.InterfaceByName/Addrs: on
	// Android, SELinux denies route table reads to non-root processes
	// ("netlinkrib: permission denied"), while the caller often already has
	// the interface details from a platform API such as VpnService. The
	// netiface ioctl path solves the same problem independently; each serves
	// as a fallback for the other.
	InterfaceAddr func(name string) string

	// Control is an optional socket control callback, called for every
	// punching-related socket (address reflection, listening, dialing, port
	// allocation) after creation and before connect or bind.
	//
	// It runs after the internal SO_REUSEADDR/SO_REUSEPORT setup. The typical
	// use is binding to a physical interface (IP_BOUND_IF / SO_BINDTODEVICE):
	// when a TUN-based proxy has taken over the default route, binding the
	// source IP alone does not choose the egress interface and punching
	// traffic loops back into the TUN device. Binding to the device makes the
	// kernel bypass the routing table and avoids the loop.
	//
	// The signature matches net.Dialer.Control and net.ListenConfig.Control,
	// so existing interface-binding helpers can be passed in directly.
	Control func(network, address string, c syscall.RawConn) error

	// PairTimeout is how long to wait for the peer to appear in the room.
	// Default 60s.
	PairTimeout time.Duration

	// PunchTimeout bounds a single punching attempt. Default 45s.
	PunchTimeout time.Duration

	// AcceptQueue is the number of established connections buffered until
	// Accept takes them. Default 64.
	//
	// Concurrency is bounded by MaxConcurrentAttempts and the candidate
	// socket budget; when this queue is full, punching blocks, which
	// propagates back-pressure to the peer.
	AcceptQueue int

	// PortScanRange is the port prediction scan window. Default 0, which
	// dials only the single port the signal server reflected for the peer.
	//
	// A symmetric NAT allocates a new, usually increasing, public port for
	// every new destination, so the reflected port differs from the one the
	// peer actually punches from and single-port dialing misses. With N > 0
	// the scanning side dials a window of N ports centered on the peer's
	// reflected port; any hit succeeds. A typical value is 512. It is the
	// legacy way to request the predictive strategy (Predictive.PortScanRange
	// is the newer field) and is only accepted on the Dialer.
	//
	// No bytes are written on the connection: the first established TCP
	// connection is used, and the upper-layer protocol detects and retries
	// a connection that turns out to be unusable.
	PortScanRange int

	// Strategy selects direct, predictive, birthday or auto. Empty keeps the
	// legacy behavior: predictive when PortScanRange > 0, direct otherwise.
	Strategy Strategy

	// StableSide declares that the Listener is behind a stable-mapping NAT
	// (endpoint-independent mapping, port cone). The birthday and auto
	// strategies require it on the Listener, and predictive assumes the
	// Listener is the stable side.
	StableSide bool

	// Predictive, Birthday and NATProbe hold per-strategy parameters.
	Predictive PredictiveOptions
	Birthday   BirthdayOptions
	NATProbe   NATProbeOptions

	// MaxConcurrentAttempts limits in-flight punching attempts per Listener
	// or Dialer. Default 2, maximum 16.
	MaxConcurrentAttempts int

	// legacyStrategy records that Strategy was omitted. Legacy deployments use
	// port_scan_range only on the Listener, so the wire strategy is selected by
	// that side while the Dialer keeps exact dialing.
	legacyStrategy bool
}

// punchOptions holds internal parameters for one punching attempt.
type punchOptions struct {
	punchTimeout time.Duration
	dialTimeout  time.Duration
	dialInterval time.Duration
	// control is the optional socket control callback from Config.Control,
	// applied to listening and dialing sockets (e.g. interface binding).
	control  func(network, address string, c syscall.RawConn) error
	debugLog DebugLogFunc
	// portScanRange is the port prediction scan window; 0 dials exactly.
	portScanRange   int
	scanConcurrency int
}

// normalize validates required fields and fills in defaults on the Dialer,
// which decides the negotiated parameters and sends them to the Listener.
func (c Config) normalize() (Config, error) {
	c, err := c.normalizeCommon()
	if err != nil {
		return c, err
	}
	return c.normalizeNegotiated()
}

// normalizeListener validates a Listener configuration.
//
// Birthday.* and the negotiated Predictive fields come from the Dialer in the
// DIAL request and would have no effect on the Listener, so setting them is
// an error: silently ignoring them would mislead the caller.
func (c Config) normalizeListener() (Config, error) {
	if !c.Birthday.isZero() {
		return c, errors.New("burrow: Config.Birthday cannot be set on a Listener; the Dialer sends it")
	}
	if !c.Predictive.negotiatedIsZero() {
		return c, errors.New("burrow: Config.Predictive.PortScanRange cannot be set on a Listener; the Dialer sends it")
	}
	if c.PortScanRange != 0 {
		return c, errors.New("burrow: Config.PortScanRange cannot be set on a Listener; the Dialer sends it")
	}
	return c.normalizeCommon()
}

// normalizeCommon handles the fields shared by both roles.
func (c Config) normalizeCommon() (Config, error) {
	if c.SignalAddr == "" {
		return c, errors.New("burrow: Config.SignalAddr is required")
	}
	if c.Room == "" {
		return c, errors.New("burrow: Config.Room is required")
	}
	if err := signalproto.ValidateRoom(c.Room); err != nil {
		return c, fmt.Errorf("burrow: invalid Config.Room: %w", err)
	}
	if c.AddressFamily == "" {
		c.AddressFamily = AddressFamilyIPv4
	}
	if c.AddressFamily != AddressFamilyIPv4 && c.AddressFamily != AddressFamilyIPv6 {
		return c, fmt.Errorf("burrow: Config.AddressFamily must be %q or %q", AddressFamilyIPv4, AddressFamilyIPv6)
	}
	if c.PairTimeout <= 0 {
		c.PairTimeout = defaultPairTimeout
	}
	if c.PunchTimeout <= 0 {
		c.PunchTimeout = defaultPunchTimeout
	}
	if c.AcceptQueue <= 0 {
		c.AcceptQueue = defaultAcceptQueue
	}
	if c.PortScanRange < 0 || c.PortScanRange > 65535 {
		return c, fmt.Errorf("burrow: Config.PortScanRange out of range: %d", c.PortScanRange)
	}
	if c.Strategy == "" {
		c.legacyStrategy = true
		if c.PortScanRange > 0 {
			c.Strategy = StrategyPredictive
		} else {
			c.Strategy = StrategyDirect
		}
	}
	if err := signalproto.ValidateStrategy(c.Strategy); err != nil {
		return c, fmt.Errorf("burrow: %w", err)
	}
	// Concurrency and AttemptTimeout are local and not negotiated; each side
	// fills in its own defaults.
	if c.Predictive.Concurrency <= 0 {
		c.Predictive.Concurrency = scanConcurrency
	}
	if c.Predictive.Concurrency > candidateSocketBudget-1 {
		return c, fmt.Errorf("burrow: predictive concurrency exceeds limit: %d", c.Predictive.Concurrency)
	}
	if c.Predictive.AttemptTimeout <= 0 {
		c.Predictive.AttemptTimeout = 2 * time.Second
	}
	if c.NATProbe.InitialSamples <= 0 {
		c.NATProbe.InitialSamples = 8
	}
	if c.NATProbe.MaxSamples <= 0 {
		c.NATProbe.MaxSamples = 24
	}
	if c.NATProbe.InitialSamples < 3 || c.NATProbe.MaxSamples < c.NATProbe.InitialSamples || c.NATProbe.MaxSamples > 24 {
		return c, errors.New("burrow: invalid NAT probe sample count")
	}
	if c.NATProbe.Timeout <= 0 {
		c.NATProbe.Timeout = 5 * time.Second
	}
	if c.NATProbe.CacheTTL <= 0 {
		c.NATProbe.CacheTTL = 10 * time.Minute
	}
	if len(c.NATProbe.Targets) == 0 {
		c.NATProbe.Targets = []string{c.SignalAddr}
	}
	if c.MaxConcurrentAttempts <= 0 {
		c.MaxConcurrentAttempts = 2
	}
	if c.MaxConcurrentAttempts > 16 {
		return c, fmt.Errorf("burrow: max concurrent attempts exceeds limit: %d", c.MaxConcurrentAttempts)
	}
	return c, nil
}

// normalizeNegotiated fills in and validates the parameters the Dialer
// decides and sends to the Listener in DIAL.
func (c Config) normalizeNegotiated() (Config, error) {
	if c.Predictive.PortScanRange <= 0 {
		if c.PortScanRange > 0 {
			c.Predictive.PortScanRange = c.PortScanRange
		} else {
			c.Predictive.PortScanRange = 512
		}
	}
	if c.Predictive.PortScanRange > 65535 {
		return c, fmt.Errorf("burrow: predictive port scan range out of range: %d", c.Predictive.PortScanRange)
	}
	if c.Birthday.Candidates <= 0 {
		c.Birthday.Candidates = 256
	}
	if c.Birthday.Candidates > 512 {
		return c, fmt.Errorf("burrow: birthday candidates exceed limit: %d", c.Birthday.Candidates)
	}
	if c.Birthday.MinPort == 0 {
		c.Birthday.MinPort = 1024
	}
	if c.Birthday.MaxPort == 0 {
		c.Birthday.MaxPort = 65535
	}
	if c.Birthday.MinPort > c.Birthday.MaxPort || int(c.Birthday.MaxPort-c.Birthday.MinPort)+1 < c.Birthday.Candidates {
		return c, errors.New("burrow: birthday port range is smaller than the candidate count")
	}
	if c.Birthday.Warmup <= 0 {
		c.Birthday.Warmup = 250 * time.Millisecond
	}
	if c.Birthday.AttemptTimeout <= 0 {
		c.Birthday.AttemptTimeout = 3 * time.Second
	}
	if c.Birthday.SuccessGrace <= 0 {
		c.Birthday.SuccessGrace = 250 * time.Millisecond
	}
	if c.Birthday.MaxSuccessCandidates <= 0 {
		c.Birthday.MaxSuccessCandidates = 8
	}
	if err := checkBirthdayTiming(c.Birthday.Warmup, c.Birthday.AttemptTimeout, c.Birthday.SuccessGrace); err != nil {
		return c, fmt.Errorf("burrow: %w", err)
	}
	if c.Birthday.MaxSuccessCandidates > 16 {
		return c, fmt.Errorf("burrow: birthday max success candidates exceeds limit: %d", c.Birthday.MaxSuccessCandidates)
	}
	if c.Strategy == StrategyBirthday || c.Strategy == StrategyAuto {
		switch c.Birthday.Selection {
		case CandidateSelectionACK, CandidateSelectionFanout:
		default:
			return c, errors.New("burrow: birthday selection must be ack or fanout")
		}
	}
	return c, nil
}

// withParameters adopts the parameters sent by the peer and returns the
// Config in effect for this attempt.
//
// Resource parameters are never clamped silently: the birthday probability
// model only holds if both sides use exactly the same candidate count and
// port space, and quietly shrinking them would turn a clear failure into a
// hard-to-diagnose low success rate. Values over the local limit reject the
// attempt so the peer gets an explicit error.
func (c Config) withParameters(p signalproto.StrategyParameters) (Config, error) {
	if p.BirthdayCandidates > 512 {
		return c, fmt.Errorf("%w: peer birthday candidates=%d exceeds local limit 512", ErrResourceLimit, p.BirthdayCandidates)
	}
	if p.PredictiveRange > 65535 {
		return c, fmt.Errorf("%w: peer predictive range=%d out of range", ErrStrategyMismatch, p.PredictiveRange)
	}
	if p.BirthdayCandidates > 0 {
		if p.BirthdayMinPort > p.BirthdayMaxPort ||
			int(p.BirthdayMaxPort-p.BirthdayMinPort)+1 < p.BirthdayCandidates {
			return c, fmt.Errorf("%w: peer birthday port range is smaller than the candidate count", ErrStrategyMismatch)
		}
	}
	if p.BirthdayMaxSuccess > 16 {
		return c, fmt.Errorf("%w: peer birthday max success=%d exceeds local limit 16", ErrResourceLimit, p.BirthdayMaxSuccess)
	}
	// The timing values are rejected rather than clamped for the same reason,
	// and because the Listener echoes the parameters in READY: a clamped
	// value would change the fingerprint and the signal server would drop the
	// READY as a strategy mismatch, leaving the Dialer to time out.
	if p.BirthdayWarmup < 0 || p.BirthdayTimeout < 0 || p.BirthdayGrace < 0 {
		return c, fmt.Errorf("%w: peer birthday timing is negative", ErrStrategyMismatch)
	}
	if err := checkBirthdayTiming(p.BirthdayWarmup, p.BirthdayTimeout, p.BirthdayGrace); err != nil {
		return c, fmt.Errorf("%w: peer %w", ErrResourceLimit, err)
	}
	// PortScanRange comes from the peer; AttemptTimeout and Concurrency stay
	// local.
	c.Predictive.PortScanRange = p.PredictiveRange
	c.Birthday.Candidates = p.BirthdayCandidates
	c.Birthday.MinPort = p.BirthdayMinPort
	c.Birthday.MaxPort = p.BirthdayMaxPort
	c.Birthday.Warmup = p.BirthdayWarmup
	c.Birthday.AttemptTimeout = p.BirthdayTimeout
	c.Birthday.SuccessGrace = p.BirthdayGrace
	c.Birthday.MaxSuccessCandidates = p.BirthdayMaxSuccess
	c.Birthday.Selection = p.BirthdaySelection
	return c, nil
}

// checkBirthdayDeadline checks, on the Listener, that a birthday attempt
// with the peer's timing (Warmup+AttemptTimeout, already adopted by
// withParameters) fits within the local PunchTimeout. It only applies once
// the negotiated strategy is birthday: direct and predictive attempts do not
// use these values and are bounded by their own timeouts.
func (c Config) checkBirthdayDeadline() error {
	total := c.Birthday.Warmup + c.Birthday.AttemptTimeout
	if c.PunchTimeout > 0 && total > c.PunchTimeout {
		return fmt.Errorf("%w: peer birthday warmup+timeout=%s exceeds local punch timeout %s",
			ErrResourceLimit, total, c.PunchTimeout)
	}
	return nil
}

// checkBirthdayTiming enforces the hard upper bounds of the birthday timing
// parameters.
func checkBirthdayTiming(warmup, attemptTimeout, successGrace time.Duration) error {
	if warmup > maxBirthdayWarmup {
		return fmt.Errorf("birthday warmup=%s exceeds limit %s", warmup, maxBirthdayWarmup)
	}
	if attemptTimeout > maxBirthdayAttemptTimeout {
		return fmt.Errorf("birthday attempt timeout=%s exceeds limit %s", attemptTimeout, maxBirthdayAttemptTimeout)
	}
	if successGrace > maxBirthdaySuccessGrace {
		return fmt.Errorf("birthday success grace=%s exceeds limit %s", successGrace, maxBirthdaySuccessGrace)
	}
	return nil
}

func (c Config) strategyParameters() signalproto.StrategyParameters {
	return signalproto.StrategyParameters{
		PredictiveRange:    c.Predictive.PortScanRange,
		BirthdayCandidates: c.Birthday.Candidates,
		BirthdayMinPort:    c.Birthday.MinPort,
		BirthdayMaxPort:    c.Birthday.MaxPort,
		BirthdayWarmup:     c.Birthday.Warmup,
		BirthdayTimeout:    c.Birthday.AttemptTimeout,
		BirthdayGrace:      c.Birthday.SuccessGrace,
		BirthdayMaxSuccess: c.Birthday.MaxSuccessCandidates,
		BirthdaySelection:  c.Birthday.Selection,
	}
}

func (c Config) punchOptions(strategy Strategy, self role) punchOptions {
	options := punchOptions{
		punchTimeout:    c.PunchTimeout,
		dialTimeout:     defaultDialTimeout,
		dialInterval:    defaultDialInterval,
		control:         c.Control,
		debugLog:        c.DebugLog,
		scanConcurrency: c.Predictive.Concurrency,
	}
	// The Listener is always the stable side: it scans the predicted window of
	// the sequential side, while the Dialer dials the Listener's stable public
	// endpoint exactly.
	if strategy == StrategyPredictive && self == roleListener {
		options.portScanRange = c.Predictive.PortScanRange
		options.punchTimeout = c.Predictive.AttemptTimeout
	}
	return options
}

func (c Config) interfaceName() string {
	if c.BindInterface != "" {
		return c.BindInterface
	}
	if c.InterfaceName != nil {
		return c.InterfaceName()
	}
	return ""
}

func (c Config) tcpNetwork() string {
	if c.AddressFamily == AddressFamilyIPv6 {
		return "tcp6"
	}
	return "tcp4"
}

// socketControl combines reuseControl (SO_REUSEADDR/SO_REUSEPORT) and extra
// (e.g. interface binding) into one control callback, running reuseControl
// first. With a nil extra it is just reuseControl.
func socketControl(extra func(network, address string, c syscall.RawConn) error) func(network, address string, c syscall.RawConn) error {
	if extra == nil {
		return reuseControl
	}
	return func(network, address string, c syscall.RawConn) error {
		if err := reuseControl(network, address, c); err != nil {
			return err
		}
		return extra(network, address, c)
	}
}
