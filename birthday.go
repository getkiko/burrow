package burrow

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	mathrand "math/rand"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type birthdayDialSpec struct {
	localAddr string
	target    string
}

func sampleBirthdayPorts(minPort, maxPort uint16, count int) ([]int, error) {
	space := int(maxPort-minPort) + 1
	if count < 1 || count > space {
		return nil, fmt.Errorf("birthday candidate count %d exceeds port space %d", count, space)
	}
	var seed [8]byte
	if _, err := cryptorand.Read(seed[:]); err != nil {
		return nil, err
	}
	rng := mathrand.New(mathrand.NewSource(int64(binary.LittleEndian.Uint64(seed[:]))))
	permutation := rng.Perm(space)
	ports := make([]int, count)
	for index := range ports {
		ports[index] = int(minPort) + permutation[index]
	}
	return ports, nil
}

func runBirthdayListener(
	ctx context.Context,
	cfg Config,
	session string,
	stableLocalAddr string,
	dialerEndpoint string,
	selectionKey string,
	ready func() error,
	yield func(net.Conn) error,
) error {
	peerIP, _, err := net.SplitHostPort(dialerEndpoint)
	if err != nil {
		return err
	}
	ports, err := sampleBirthdayPorts(cfg.Birthday.MinPort, cfg.Birthday.MaxPort, cfg.Birthday.Candidates)
	if err != nil {
		return err
	}
	specs := make([]birthdayDialSpec, 0, len(ports))
	for _, port := range ports {
		specs = append(specs, birthdayDialSpec{
			localAddr: stableLocalAddr,
			target:    net.JoinHostPort(peerIP, strconv.Itoa(port)),
		})
	}

	attemptCtx, cancel := context.WithTimeout(ctx, cfg.Birthday.Warmup+cfg.Birthday.AttemptTimeout)
	defer cancel()
	hits, armed, done := launchBirthdayDialSet(attemptCtx, specs, cfg.Control)
	if err := waitBirthdayArmed(attemptCtx, armed, len(specs)); err != nil {
		cancel()
		<-done
		drainBirthdayHits(hits)
		return err
	}
	warmup := time.NewTimer(cfg.Birthday.Warmup)
	select {
	case <-warmup.C:
	case <-attemptCtx.Done():
		stopTimer(warmup)
		<-done
		drainBirthdayHits(hits)
		return fmt.Errorf("birthday candidate warmup timed out: %w", ErrPunchFailed)
	}
	if err := ready(); err != nil {
		cancel()
		<-done
		drainBirthdayHits(hits)
		return err
	}

	var result error
	if cfg.Birthday.Selection == CandidateSelectionACK {
		result = collectBirthdayACK(attemptCtx, hits, session, selectionKey, yield)
	} else {
		result = collectBirthdayFanout(attemptCtx, hits, cfg.Birthday.SuccessGrace, cfg.Birthday.MaxSuccessCandidates, yield)
	}
	cancel()
	<-done
	drainBirthdayHits(hits)
	return result
}

func runBirthdayDialer(ctx context.Context, cfg Config, session, localAddr, stableEndpoint, selectionKey string) (net.Conn, error) {
	localIP, _, err := net.SplitHostPort(localAddr)
	if err != nil {
		return nil, err
	}
	specs := make([]birthdayDialSpec, cfg.Birthday.Candidates)
	for index := range specs {
		specs[index] = birthdayDialSpec{
			localAddr: net.JoinHostPort(localIP, "0"),
			target:    stableEndpoint,
		}
	}
	attemptCtx, cancel := context.WithTimeout(ctx, cfg.Birthday.AttemptTimeout)
	defer cancel()
	hits, armed, done := launchBirthdayDialSet(attemptCtx, specs, cfg.Control)
	if err := waitBirthdayArmed(attemptCtx, armed, len(specs)); err != nil {
		cancel()
		<-done
		drainBirthdayHits(hits)
		return nil, err
	}

	var conn net.Conn
	if cfg.Birthday.Selection == CandidateSelectionACK {
		conn, err = collectDialerACK(attemptCtx, hits, session, selectionKey)
	} else {
		select {
		case conn = <-hits:
		case <-attemptCtx.Done():
			err = fmt.Errorf("birthday punch to %s timed out: %w", stableEndpoint, ErrPunchFailed)
		}
	}
	cancel()
	<-done
	drainBirthdayHits(hits)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, fmt.Errorf("birthday punch to %s failed: %w", stableEndpoint, ErrPunchFailed)
	}
	return conn, nil
}

func launchBirthdayDialSet(ctx context.Context, specs []birthdayDialSpec, control func(network, address string, c syscall.RawConn) error) (<-chan net.Conn, <-chan int, <-chan struct{}) {
	sdkSpecs := make([]BirthdayDialSpec, len(specs))
	for index, spec := range specs {
		sdkSpecs[index] = BirthdayDialSpec{LocalAddr: spec.localAddr, Target: spec.target}
	}
	dialSet := LaunchBirthdayDialSet(ctx, sdkSpecs, control)
	return dialSet.Connections, dialSet.Armed, dialSet.Done
}

func waitBirthdayArmed(ctx context.Context, armed <-chan int, expected int) error {
	select {
	case count := <-armed:
		if count != expected {
			return fmt.Errorf("birthday armed %d/%d candidates: %w", count, expected, ErrResourceLimit)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("birthday candidate arming failed: %w", ctx.Err())
	}
}

func collectBirthdayACK(ctx context.Context, hits <-chan net.Conn, session, selectionKey string, yield func(net.Conn) error) error {
	for {
		select {
		case conn := <-hits:
			if err := serverSelectCandidate(ctx, conn, session, selectionKey); err != nil {
				_ = conn.Close()
				continue
			}
			if err := yield(conn); err != nil {
				_ = conn.Close()
				return err
			}
			return nil
		case <-ctx.Done():
			return fmt.Errorf("birthday candidate selection timed out: %w", ErrPunchFailed)
		}
	}
}

func collectDialerACK(ctx context.Context, hits <-chan net.Conn, session, selectionKey string) (net.Conn, error) {
	selectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	winner := make(chan net.Conn, 1)
	var handshakes sync.WaitGroup
	defer handshakes.Wait()
	for {
		select {
		case conn := <-hits:
			handshakes.Go(func() {
				if err := clientSelectCandidate(selectionCtx, conn, session, selectionKey); err != nil {
					_ = conn.Close()
					return
				}
				select {
				case winner <- conn:
				case <-selectionCtx.Done():
					_ = conn.Close()
				default:
					_ = conn.Close()
				}
			})
		case conn := <-winner:
			cancel()
			return conn, nil
		case <-ctx.Done():
			select {
			case conn := <-winner:
				return conn, nil
			default:
				return nil, fmt.Errorf("birthday candidate selection timed out: %w", ErrSelectionFailed)
			}
		}
	}
}

func collectBirthdayFanout(ctx context.Context, hits <-chan net.Conn, grace time.Duration, maxCandidates int, yield func(net.Conn) error) error {
	delivered := 0
	var timer *time.Timer
	var timerCh <-chan time.Time
	defer func() {
		if timer != nil {
			stopTimer(timer)
		}
	}()
	for {
		select {
		case conn := <-hits:
			if err := yield(conn); err != nil {
				_ = conn.Close()
				return err
			}
			delivered++
			if delivered == 1 {
				timer = time.NewTimer(grace)
				timerCh = timer.C
			}
			if delivered >= maxCandidates {
				return nil
			}
		case <-timerCh:
			return nil
		case <-ctx.Done():
			if delivered > 0 {
				return nil
			}
			return fmt.Errorf("birthday collision timed out: %w", ErrPunchFailed)
		}
	}
}

func drainBirthdayHits(hits <-chan net.Conn) {
	for {
		select {
		case conn := <-hits:
			_ = conn.Close()
		default:
			return
		}
	}
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
