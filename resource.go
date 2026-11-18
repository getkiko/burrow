package burrow

import (
	"context"
	"sync"
)

const candidateSocketBudget = 512

type attemptLimiter struct {
	sessions chan struct{}

	socketMu      sync.Mutex
	socketUsed    int
	socketChanged chan struct{}
}

func newAttemptLimiter(maxSessions int) *attemptLimiter {
	return &attemptLimiter{
		sessions:      make(chan struct{}, maxSessions),
		socketChanged: make(chan struct{}),
	}
}

func (l *attemptLimiter) acquireSession(ctx context.Context) (func(), error) {
	select {
	case l.sessions <- struct{}{}:
		return func() { <-l.sessions }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *attemptLimiter) acquireSockets(ctx context.Context, count int) (func(), error) {
	if count < 1 {
		count = 1
	}
	for {
		l.socketMu.Lock()
		if l.socketUsed+count <= candidateSocketBudget {
			l.socketUsed += count
			l.socketMu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					l.socketMu.Lock()
					l.socketUsed -= count
					close(l.socketChanged)
					l.socketChanged = make(chan struct{})
					l.socketMu.Unlock()
				})
			}, nil
		}
		changed := l.socketChanged
		l.socketMu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// strategySocketWeight returns how many candidate sockets a strategy
// reserves. It takes plain numbers rather than the TCP Config so the UDP path,
// which has the same birthday/predictive semantics, can reuse it.
func strategySocketWeight(strategy Strategy, birthdayCandidates, predictiveConcurrency int) int {
	switch strategy {
	case StrategyBirthday:
		return birthdayCandidates
	case StrategyPredictive:
		return predictiveConcurrency + 1
	default:
		return 2
	}
}
