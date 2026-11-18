// Package tunnel is a standalone TCP port-forwarding tunnel built on burrow's
// hole punching.
//
// The client listens on a local TCP address and punches one hole per accepted
// connection (one-to-one); the server forwards every punched hole to a fixed
// local TCP address.
//
// Both ends are plain pipes: a hole carries raw bytes with no custom header,
// handshake or keepalive. There is deliberately no multiplexing, because a
// multiplexing handshake would change the traffic pattern. The client splices
// each accepted TCP connection onto its hole without parsing anything. Target
// addressing is entirely up to the upper-layer protocol; this package knows
// nothing about the bytes it carries.
package tunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/getkiko/burrow"
	"github.com/getkiko/burrow/internal/netiface"
)

// Duration is a time.Duration that is written in JSON as a string such as
// "250ms" or "3s".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"250ms\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// BirthdayOptions holds the birthday strategy parameters. They may only be
// set on the client: the Dialer sends them to the Listener with DIAL.
type BirthdayOptions struct {
	Selection            string   `json:"selection"`
	Candidates           int      `json:"candidates"`
	MinPort              uint16   `json:"min_port"`
	MaxPort              uint16   `json:"max_port"`
	Warmup               Duration `json:"warmup"`
	AttemptTimeout       Duration `json:"attempt_timeout"`
	SuccessGrace         Duration `json:"success_grace"`
	MaxSuccessCandidates int      `json:"max_success_candidates"`
}

func (o BirthdayOptions) isZero() bool {
	return o == BirthdayOptions{}
}

// PredictiveOptions holds the predictive strategy parameters.
//
// PortScanRange is negotiated and may only be set on the client. Concurrency
// and AttemptTimeout are purely local resource limits; each side may set its
// own values.
type PredictiveOptions struct {
	PortScanRange  int      `json:"port_scan_range"`
	Concurrency    int      `json:"concurrency"`
	AttemptTimeout Duration `json:"attempt_timeout"`
}

// commonConfig holds the fields shared by client and server.
type commonConfig struct {
	Signal        string            `json:"signal"`
	Room          string            `json:"room"`
	BindInterface string            `json:"bind_interface"`
	Strategy      string            `json:"strategy"`
	Mode          string            `json:"mode"`
	Hosts         map[string]string `json:"hosts"`
	LogLevel      string            `json:"log_level"`

	// LogSink is injected by the caller to route logs to the host's UI (for
	// example an Android app).
	//
	// It is tagged json:"-" because it is a runtime dependency set in code,
	// not a user setting. When nil, logs go to stderr only.
	LogSink LogSink `json:"-"`

	MaxConcurrentAttempts int               `json:"max_concurrent_attempts"`
	Predictive            PredictiveOptions `json:"predictive"`
}

// ClientConfig is the client-side configuration.
type ClientConfig struct {
	commonConfig
	// Listen is the local TCP listen address. Each accepted connection gets
	// its own hole and is forwarded as-is without any protocol parsing; what
	// runs on top (SOCKS5 or anything else) is up to the user.
	Listen string `json:"listen"`

	// Trials is the maximum number of consecutive punch rounds per
	// connection. Zero or unset means the default of 100.
	//
	// The birthday strategy relies on a birthday collision: a single round
	// succeeds far less than 100% of the time, and the overall success rate
	// comes from many rounds (compare -trials of `burrow birthday`, which
	// also defaults to 100). Raise it for difficult NAT combinations, such as
	// one side behind a strict cone NAT.
	//
	// Client only: the server accepts passively and never retries.
	Trials int `json:"trials"`

	Birthday BirthdayOptions `json:"birthday"`
}

// ServerConfig is the server-side configuration.
//
// It has no Birthday field: negotiated parameters come from the client, and
// burrow.Listen rejects them on the listener side. LoadServerConfig first
// parses the file into a map to catch such misconfiguration with a clear
// error.
type ServerConfig struct {
	commonConfig
	Forward    string `json:"forward"`
	StableSide bool   `json:"stable_side"`

	// SkipNATCheck skips the NAT self-check performed before startup.
	//
	// The self-check refuses to run a server behind a symmetric NAT, where
	// hole punching almost never succeeds. Set it to true only if you know
	// the detection is wrong or want to try anyway.
	SkipNATCheck bool `json:"skip_nat_check"`
}

// LoadClientConfig reads and validates a client configuration file.
func LoadClientConfig(path string) (ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ClientConfig{}, fmt.Errorf("read config file: %w", err)
	}
	var cfg ClientConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ClientConfig{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return ClientConfig{}, err
	}
	return cfg, nil
}

// LoadServerConfig reads and validates a server configuration file.
func LoadServerConfig(path string) (ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ServerConfig{}, fmt.Errorf("read config file: %w", err)
	}
	// Check for negotiated fields via a map first: they are not part of
	// ServerConfig, so plain unmarshalling would silently ignore them and the
	// user would think they took effect.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return ServerConfig{}, fmt.Errorf("parse config: %w", err)
	}
	if _, ok := raw["birthday"]; ok {
		return ServerConfig{}, errors.New(
			"birthday is not allowed in a server config: it is sent by the client and must be set in the client config")
	}
	if predictiveRaw, ok := raw["predictive"]; ok {
		var probe struct {
			PortScanRange *int `json:"port_scan_range"`
		}
		if err := json.Unmarshal(predictiveRaw, &probe); err != nil {
			return ServerConfig{}, fmt.Errorf("parse predictive: %w", err)
		}
		if probe.PortScanRange != nil {
			return ServerConfig{}, errors.New(
				"predictive.port_scan_range is not allowed in a server config: it is sent by the client and must be set in the client config")
		}
	}

	var cfg ServerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ServerConfig{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return ServerConfig{}, err
	}
	return cfg, nil
}

// validateCommon validates the shared fields and fills in defaults.
func (c *commonConfig) validateCommon() error {
	if c.Signal == "" {
		return errors.New("config is missing required field signal")
	}
	if c.Room == "" {
		return errors.New("config is missing required field room")
	}
	if c.BindInterface == "" {
		return errors.New("config is missing required field bind_interface: " +
			"with 0.0.0.0 the kernel may pick an unroutable source address and punching will fail, so the interface must be explicit")
	}
	if c.Strategy == "" {
		return errors.New("config is missing required field strategy: one of direct / predictive / birthday")
	}
	switch c.Strategy {
	case "direct", "predictive", "birthday":
	case "auto":
		return errors.New("strategy=auto is not supported: set direct / predictive / birthday explicitly")
	default:
		return fmt.Errorf("invalid strategy %q: one of direct / predictive / birthday", c.Strategy)
	}
	switch c.Mode {
	case "", "header":
	default:
		return fmt.Errorf("invalid mode %q: empty or header", c.Mode)
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log_level %q: one of debug / info / warn / error", c.LogLevel)
	}
	return nil
}

// Validate validates the configuration and fills in defaults.
//
// It is exported so callers (such as the mobile bindings) can validate a
// configuration without reading a file or starting anything, using the same
// rules as startup, so a config never passes validation and then fails to
// start.
func (c *ClientConfig) Validate() error {
	if c.Listen == "" {
		return errors.New("config is missing required field listen")
	}
	if err := c.validateCommon(); err != nil {
		return err
	}
	if c.Trials < 0 {
		return fmt.Errorf("trials must not be negative: %d", c.Trials)
	}
	if c.Trials == 0 {
		c.Trials = defaultTrials
	}
	if c.Strategy == "birthday" {
		switch c.Birthday.Selection {
		case "ack", "fanout":
		case "":
			return errors.New("birthday.selection is required with strategy=birthday: one of ack / fanout")
		default:
			return fmt.Errorf("invalid birthday.selection %q: one of ack / fanout", c.Birthday.Selection)
		}
	}
	return nil
}

// Validate validates the configuration and fills in defaults.
//
// It is exported so callers (such as the mobile bindings) can validate a
// configuration without reading a file or starting anything, using the same
// rules as startup, so a config never passes validation and then fails to
// start.
func (c *ServerConfig) Validate() error {
	if c.Forward == "" {
		return errors.New("config is missing required field forward")
	}
	if err := c.validateCommon(); err != nil {
		return err
	}
	// burrow.Listen requires StableSide with the birthday strategy; failing
	// here gives a clearer error.
	if c.Strategy == "birthday" && !c.StableSide {
		return errors.New("strategy=birthday requires stable_side: true on the server")
	}
	return nil
}

// toCommonBurrowConfig builds the part of the SDK config shared by both sides.
func (c commonConfig) toCommonBurrowConfig() burrow.Config {
	cfg := burrow.Config{
		SignalAddr:            c.Signal,
		Room:                  c.Room,
		BindInterface:         c.BindInterface,
		Mode:                  c.Mode,
		MaxConcurrentAttempts: c.MaxConcurrentAttempts,
		// BindInterface only selects the source IP; it does not pick the
		// egress interface. When a global TUN proxy owns the default route
		// (common on phones running a VPN app), the kernel still routes
		// packets into the TUN device. Control binds the socket to the device
		// (SO_BINDTOIFINDEX) so the routing table is bypassed.
		//
		// Without it the punching sockets still work, because BindInterface
		// picks the right source IP, but the signaling WebSocket gets captured
		// by the TUN: the server then sees the proxy's egress IP instead of
		// this host's public IP, and every punch candidate is wrong.
		Control: netiface.BindControl(c.BindInterface),
	}
	switch c.Strategy {
	case "direct":
		cfg.Strategy = burrow.StrategyDirect
	case "predictive":
		cfg.Strategy = burrow.StrategyPredictive
	case "birthday":
		cfg.Strategy = burrow.StrategyBirthday
	}
	// Concurrency and AttemptTimeout are local and may be set on both sides.
	cfg.Predictive.Concurrency = c.Predictive.Concurrency
	cfg.Predictive.AttemptTimeout = time.Duration(c.Predictive.AttemptTimeout)
	return cfg
}

// ToBurrowConfig converts the client configuration into an SDK config,
// including all negotiated parameters.
func (c ClientConfig) ToBurrowConfig() burrow.Config {
	cfg := c.toCommonBurrowConfig()
	cfg.Predictive.PortScanRange = c.Predictive.PortScanRange
	if !c.Birthday.isZero() {
		cfg.Birthday = burrow.BirthdayOptions{
			Candidates:           c.Birthday.Candidates,
			MinPort:              c.Birthday.MinPort,
			MaxPort:              c.Birthday.MaxPort,
			Warmup:               time.Duration(c.Birthday.Warmup),
			AttemptTimeout:       time.Duration(c.Birthday.AttemptTimeout),
			SuccessGrace:         time.Duration(c.Birthday.SuccessGrace),
			MaxSuccessCandidates: c.Birthday.MaxSuccessCandidates,
		}
		switch c.Birthday.Selection {
		case "ack":
			cfg.Birthday.Selection = burrow.CandidateSelectionACK
		case "fanout":
			cfg.Birthday.Selection = burrow.CandidateSelectionFanout
		}
	}
	return cfg
}

// ToBurrowConfig converts the server configuration into an SDK config.
//
// It deliberately sets no negotiated fields (Birthday,
// Predictive.PortScanRange): burrow.Listen rejects configs that carry them.
func (c ServerConfig) ToBurrowConfig() burrow.Config {
	cfg := c.toCommonBurrowConfig()
	cfg.StableSide = c.StableSide
	return cfg
}
