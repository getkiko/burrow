// Package birthday contains the portable state and probability model for the
// birthday-collision TCP hole-punch prototype.
package birthdaylab

import (
	"fmt"
	"math"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

const MaxCandidates = 512

// TrialResult is the complete observable state of one collision trial.
type TrialResult struct {
	Success     bool
	Session     string
	LocalAddr   string
	RemoteAddr  string
	Elapsed     time.Duration
	Diagnostics AttemptDiagnostics
}

// AttemptDiagnostics describes how many candidates reached each stage of one
// birthday attempt. It separates a probabilistic miss from local resource and
// connection-validation failures.
type AttemptDiagnostics struct {
	Requested        int
	Armed            int
	Connected        int
	Validated        int
	ValidationErrors int
	DialErrors       DialErrorCounts
}

type DialErrorCounts struct {
	Timeout  int
	Canceled int
	Refused  int
	Resource int
	Other    int
}

// TrialParameters are the birthday fields sent by the Dialer and adopted
// by the Listener for one session.
type TrialParameters struct {
	Candidates     int
	MinPort        int
	MaxPort        int
	Warmup         time.Duration
	AttemptTimeout time.Duration
}

func NewTrialParameters(candidates, minPort, maxPort int, warmup, attemptTimeout time.Duration) (TrialParameters, error) {
	parameters := TrialParameters{
		Candidates:     candidates,
		MinPort:        minPort,
		MaxPort:        maxPort,
		Warmup:         warmup,
		AttemptTimeout: attemptTimeout,
	}
	if candidates < 1 || candidates > MaxCandidates {
		return TrialParameters{}, fmt.Errorf("birthday candidates must be within 1-%d", MaxCandidates)
	}
	if minPort < 1 || maxPort > 65535 || minPort > maxPort || maxPort-minPort+1 < candidates {
		return TrialParameters{}, fmt.Errorf("birthday port range is smaller than the candidate count")
	}
	if warmup <= 0 || attemptTimeout <= 0 {
		return TrialParameters{}, fmt.Errorf("birthday warmup and attempt-timeout must be greater than 0")
	}
	return parameters, nil
}

func TrialParametersFromSignal(parameters signalproto.StrategyParameters) (TrialParameters, error) {
	if parameters.BirthdaySelection != signalproto.SelectionFanout {
		return TrialParameters{}, fmt.Errorf("birthday selection must be %s", signalproto.SelectionFanout)
	}
	return NewTrialParameters(
		parameters.BirthdayCandidates,
		int(parameters.BirthdayMinPort),
		int(parameters.BirthdayMaxPort),
		parameters.BirthdayWarmup,
		parameters.BirthdayTimeout,
	)
}

func (p TrialParameters) SignalParameters() signalproto.StrategyParameters {
	return signalproto.StrategyParameters{
		BirthdayCandidates: p.Candidates,
		BirthdayMinPort:    uint16(p.MinPort),
		BirthdayMaxPort:    uint16(p.MaxPort),
		BirthdayWarmup:     p.Warmup,
		BirthdayTimeout:    p.AttemptTimeout,
		BirthdaySelection:  signalproto.SelectionFanout,
	}
}

// Statistics accumulates experiment results without performing any I/O.
type Statistics struct {
	Trials       int
	Successes    int
	TotalElapsed time.Duration
}

func (s Statistics) Add(result TrialResult) Statistics {
	s.Trials++
	if result.Success {
		s.Successes++
	}
	s.TotalElapsed += result.Elapsed
	return s
}

func (s Statistics) SuccessRate() float64 {
	if s.Trials == 0 {
		return 0
	}
	return float64(s.Successes) / float64(s.Trials)
}

func (s Statistics) AverageElapsed() time.Duration {
	if s.Trials == 0 {
		return 0
	}
	return s.TotalElapsed / time.Duration(s.Trials)
}

// CollisionProbability returns the ideal probability that a random probe set
// intersects the client's independently allocated public-port set.
func CollisionProbability(serverProbes, clientMappings, portCount int) float64 {
	if serverProbes <= 0 || clientMappings <= 0 || portCount <= 0 {
		return 0
	}
	return 1 - math.Exp(-float64(serverProbes*clientMappings)/float64(portCount))
}

func RepeatedProbability(single float64, attempts int) float64 {
	if single <= 0 || attempts <= 0 {
		return 0
	}
	if single >= 1 {
		return 1
	}
	return 1 - math.Pow(1-single, float64(attempts))
}
