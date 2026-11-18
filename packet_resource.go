package burrow

import (
	"context"

	"golang.org/x/time/rate"
)

// This file implements resource limits for the UDP packet API.
//
// The total UDP socket budget is the shared candidateSocketBudget (512),
// enforced through the existing attemptLimiter, acquireSockets and
// strategySocketWeight (see resource.go). Only two UDP-specific limits are
// added:
//   - the predictive UDP port range is capped at 4096
//     (predictiveUDPMaxPortRange, checked during normalize);
//   - candidate packets are rate limited to 4096 packets/s with a burst
//     of 1024.

// candidatePacketLimiter limits the candidate packet rate of one dialer or
// listener. It is a token bucket from golang.org/x/time/rate, which maps
// directly to "N packets per second with a burst of B".
type candidatePacketLimiter struct {
	limiter *rate.Limiter
}

func newCandidatePacketLimiter() *candidatePacketLimiter {
	return &candidatePacketLimiter{
		limiter: rate.NewLimiter(rate.Limit(candidatePacketRateLimit), candidatePacketBurst),
	}
}

// wait blocks until the next candidate packet may be sent or ctx is done.
func (l *candidatePacketLimiter) wait(ctx context.Context) error {
	return l.limiter.Wait(ctx)
}

// predictivePortBudget returns the number of candidate sockets the
// predictive strategy must reserve: concurrency+1 on the scanning side
// (including the one aimed at the stable endpoint). It reuses
// strategySocketWeight so the accounting matches the TCP path.
func predictivePortBudget(concurrency int) int {
	return strategySocketWeight(StrategyPredictive, 0, concurrency)
}

// directPortBudget returns the number of candidate sockets the direct
// strategy must reserve.
func directPortBudget() int {
	return strategySocketWeight(StrategyDirect, 0, 0)
}
