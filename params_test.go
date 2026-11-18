package burrow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

func defaultBirthdayParameters() signalproto.StrategyParameters {
	return signalproto.StrategyParameters{
		PredictiveRange:    512,
		BirthdayCandidates: 256,
		BirthdayMinPort:    1024,
		BirthdayMaxPort:    65535,
		BirthdayWarmup:     250 * time.Millisecond,
		BirthdayTimeout:    3 * time.Second,
		BirthdayGrace:      250 * time.Millisecond,
		BirthdayMaxSuccess: 8,
		BirthdaySelection:  signalproto.SelectionACK,
	}
}

// The default parameters a Dialer sends must still be accepted.
func TestWithParametersAcceptsDefaults(t *testing.T) {
	listener := Config{PunchTimeout: defaultPunchTimeout}
	params := defaultBirthdayParameters()
	cfg, err := listener.withParameters(params)
	if err != nil {
		t.Fatalf("default parameters rejected: %v", err)
	}
	if cfg.Birthday.Warmup != params.BirthdayWarmup || cfg.Birthday.AttemptTimeout != params.BirthdayTimeout || cfg.Birthday.SuccessGrace != params.BirthdayGrace {
		t.Fatalf("timing not adopted: %+v", cfg.Birthday)
	}
	// The limits themselves are inclusive.
	params.BirthdayWarmup = maxBirthdayWarmup
	params.BirthdayTimeout = maxBirthdayAttemptTimeout
	params.BirthdayGrace = maxBirthdaySuccessGrace
	if _, err := listener.withParameters(params); err != nil {
		t.Fatalf("parameters at the limits rejected: %v", err)
	}
}

// A malicious Dialer must not be able to make the Listener hold its armed
// birthday sockets for an unbounded time.
func TestWithParametersRejectsUnboundedBirthdayTiming(t *testing.T) {
	listener := Config{PunchTimeout: defaultPunchTimeout}
	tests := []struct {
		name   string
		mutate func(*signalproto.StrategyParameters)
		want   error
	}{
		{name: "huge warmup", mutate: func(p *signalproto.StrategyParameters) { p.BirthdayWarmup = 24 * time.Hour }, want: ErrResourceLimit},
		{name: "warmup over limit", mutate: func(p *signalproto.StrategyParameters) { p.BirthdayWarmup = maxBirthdayWarmup + time.Millisecond }, want: ErrResourceLimit},
		{name: "huge timeout", mutate: func(p *signalproto.StrategyParameters) { p.BirthdayTimeout = time.Duration(1<<62 + 1) }, want: ErrResourceLimit},
		{name: "huge grace", mutate: func(p *signalproto.StrategyParameters) { p.BirthdayGrace = time.Hour }, want: ErrResourceLimit},
		{name: "negative warmup", mutate: func(p *signalproto.StrategyParameters) { p.BirthdayWarmup = -time.Second }, want: ErrStrategyMismatch},
		{name: "negative timeout", mutate: func(p *signalproto.StrategyParameters) { p.BirthdayTimeout = -1 }, want: ErrStrategyMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := defaultBirthdayParameters()
			test.mutate(&params)
			if _, err := listener.withParameters(params); !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
		})
	}
}

// Warmup+AttemptTimeout is bounded by the Listener's own PunchTimeout, but
// only for birthday: a short PunchTimeout must not reject direct or
// predictive dials, which carry the default birthday values too.
func TestBirthdayDeadlineOnlyAppliesToBirthday(t *testing.T) {
	listener := Config{PunchTimeout: 2 * time.Second}
	params := defaultBirthdayParameters() // 250ms + 3s > 2s
	cfg, err := listener.withParameters(params)
	if err != nil {
		t.Fatalf("withParameters must not reject on the local punch timeout: %v", err)
	}
	if err := cfg.checkBirthdayDeadline(); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("birthday deadline: err = %v, want ErrResourceLimit", err)
	}

	listener.PunchTimeout = 5 * time.Second
	if cfg, err = listener.withParameters(params); err != nil {
		t.Fatal(err)
	}
	if err := cfg.checkBirthdayDeadline(); err != nil {
		t.Fatalf("defaults within the local punch timeout rejected: %v", err)
	}
}

// End to end on the Listener: with PunchTimeout below the birthday defaults,
// a predictive request still gets past parameter and strategy checks, while
// a birthday request is refused with ErrResourceLimit before any socket is
// allocated.
func TestEstablishListenerDeadlineCheckIsBirthdayOnly(t *testing.T) {
	params := defaultBirthdayParameters()
	base := Config{
		PunchTimeout: 2 * time.Second,
		Strategy:     StrategyAuto,
		StableSide:   true,
	}
	birthday := signalproto.Message{
		Session:     "sess-birthday",
		Strategy:    StrategyBirthday,
		Parameters:  params,
		Fingerprint: params.Fingerprint(),
	}
	err := establishListener(context.Background(), base, nil, birthday, nil, nil)
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("birthday: err = %v, want ErrResourceLimit", err)
	}

	cfg, err := base.withParameters(params)
	if err != nil {
		t.Fatal(err)
	}
	predictive := birthday
	predictive.Strategy = StrategyPredictive
	strategy, err := selectIncomingStrategy(cfg, predictive)
	if err != nil || strategy != StrategyPredictive {
		t.Fatalf("predictive: strategy = %q, err = %v", strategy, err)
	}
}

// The Dialer refuses to send timing values its peer would reject.
func TestNormalizeNegotiatedRejectsBirthdayTimingOverLimit(t *testing.T) {
	base := Config{Strategy: StrategyBirthday, Birthday: BirthdayOptions{Selection: CandidateSelectionACK}}
	if _, err := base.normalizeNegotiated(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	for _, mutate := range []func(*BirthdayOptions){
		func(o *BirthdayOptions) { o.Warmup = time.Minute },
		func(o *BirthdayOptions) { o.AttemptTimeout = time.Minute },
		func(o *BirthdayOptions) { o.SuccessGrace = time.Minute },
	} {
		cfg := base
		mutate(&cfg.Birthday)
		if _, err := cfg.normalizeNegotiated(); err == nil {
			t.Fatalf("over-limit birthday timing accepted: %+v", cfg.Birthday)
		}
	}
}

// A room name the signal server would reject fails at configuration time on
// both roles instead of being retried forever at run time.
func TestConfigRejectsRoomTheServerRejects(t *testing.T) {
	for _, room := range []string{"   ", strings.Repeat("r", signalproto.MaxRoomLength+1)} {
		cfg := Config{SignalAddr: "127.0.0.1:1", Room: room}
		if ln, err := Listen(cfg); err == nil {
			_ = ln.Close()
			t.Fatalf("Listen accepted room %q", room)
		} else if !strings.Contains(err.Error(), "Config.Room") {
			t.Fatalf("Listen error for room %q does not name the field: %v", room, err)
		}
		if d, err := NewDialer(cfg); err == nil {
			_ = d.Close()
			t.Fatalf("NewDialer accepted room %q", room)
		}
		if _, err := (PacketConfig{SignalAddr: "127.0.0.1:1", Room: room}).normalize(); err == nil {
			t.Fatalf("PacketConfig accepted room %q", room)
		}
	}
	ok := Config{SignalAddr: "127.0.0.1:1", Room: strings.Repeat("r", signalproto.MaxRoomLength)}
	if _, err := ok.normalizeCommon(); err != nil {
		t.Fatalf("a %d-byte room must be accepted: %v", signalproto.MaxRoomLength, err)
	}
}
