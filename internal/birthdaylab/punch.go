package birthdaylab

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/getkiko/burrow"
)

type PortSampler struct {
	mu  sync.Mutex
	rng *mathrand.Rand
}

func NewPortSampler() (*PortSampler, error) {
	var seedBytes [8]byte
	if _, err := cryptorand.Read(seedBytes[:]); err != nil {
		return nil, err
	}
	seed := int64(binary.LittleEndian.Uint64(seedBytes[:]))
	return &PortSampler{rng: mathrand.New(mathrand.NewSource(seed))}, nil
}

func (s *PortSampler) Sample(minPort, maxPort, count int) ([]int, error) {
	portCount := maxPort - minPort + 1
	if minPort <= 0 || maxPort > 65535 || minPort > maxPort {
		return nil, fmt.Errorf("invalid port range: %d-%d", minPort, maxPort)
	}
	if count <= 0 || count > portCount {
		return nil, fmt.Errorf("candidate count %d exceeds port range size %d", count, portCount)
	}
	s.mu.Lock()
	permutation := s.rng.Perm(portCount)
	s.mu.Unlock()
	ports := make([]int, count)
	for index := range ports {
		ports[index] = minPort + permutation[index]
	}
	return ports, nil
}

type dialSpec struct {
	localAddr string
	target    string
}

type dialHit struct {
	localAddr  string
	remoteAddr string
}

type dialControlFunc func(network, address string, conn syscall.RawConn) error

type attemptCounters struct {
	dialSet          *burrow.BirthdayDialSet
	validated        atomic.Int64
	validationErrors atomic.Int64
}

func (c *attemptCounters) snapshot() AttemptDiagnostics {
	diagnostics := c.dialSet.Diagnostics()
	return AttemptDiagnostics{
		Requested:        diagnostics.Requested,
		Armed:            diagnostics.Armed,
		Connected:        diagnostics.Connected,
		Validated:        int(c.validated.Load()),
		ValidationErrors: int(c.validationErrors.Load()),
		DialErrors: DialErrorCounts{
			Timeout:  diagnostics.DialErrors.Timeout,
			Canceled: diagnostics.DialErrors.Canceled,
			Refused:  diagnostics.DialErrors.Refused,
			Resource: diagnostics.DialErrors.Resource,
			Other:    diagnostics.DialErrors.Other,
		},
	}
}

// RunServerAttempt arms all random outbound SYNs before publishing the stable
// endpoint through READY. The warmup window gives the sockets time to enter
// SYN_SENT before the client creates its target-dependent mappings.
func RunServerAttempt(
	parent context.Context,
	sampler *PortSampler,
	session string,
	stableLocalAddr string,
	clientPublicIP string,
	minPort int,
	maxPort int,
	candidates int,
	warmup time.Duration,
	attemptTimeout time.Duration,
	ready func() error,
) (TrialResult, error) {
	startedAt := time.Now()
	ports, err := sampler.Sample(minPort, maxPort, candidates)
	if err != nil {
		return TrialResult{}, err
	}
	specs := make([]dialSpec, 0, len(ports))
	for _, port := range ports {
		specs = append(specs, dialSpec{
			localAddr: stableLocalAddr,
			target:    net.JoinHostPort(clientPublicIP, strconv.Itoa(port)),
		})
	}

	ctx, cancel := context.WithTimeout(parent, warmup+attemptTimeout)
	defer cancel()
	hits, armed, done, counters := launchDialSet(ctx, specs, session)
	var armedCount int
	select {
	case armedCount = <-armed:
	case <-ctx.Done():
		<-done
		return TrialResult{
			Session: session, Elapsed: time.Since(startedAt), Diagnostics: counters.snapshot(),
		}, ctx.Err()
	}
	if armedCount != candidates {
		cancel()
		<-done
		return TrialResult{
			Session: session, Elapsed: time.Since(startedAt), Diagnostics: counters.snapshot(),
		}, fmt.Errorf("birthday armed %d/%d candidates", armedCount, candidates)
	}

	timer := time.NewTimer(warmup)
	select {
	case <-timer.C:
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		<-done
		return TrialResult{
			Session: session, Elapsed: time.Since(startedAt), Diagnostics: counters.snapshot(),
		}, ctx.Err()
	}
	if err := ready(); err != nil {
		cancel()
		<-done
		return TrialResult{
			Session: session, Elapsed: time.Since(startedAt), Diagnostics: counters.snapshot(),
		}, err
	}

	result := awaitHit(ctx, session, hits, startedAt)
	cancel()
	<-done
	result.Diagnostics = counters.snapshot()
	return result, nil
}

func RunClientAttempt(
	parent context.Context,
	session string,
	bindIP string,
	serverPublicAddress string,
	mappings int,
	attemptTimeout time.Duration,
) TrialResult {
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(parent, attemptTimeout)
	defer cancel()
	specs := make([]dialSpec, mappings)
	for index := range specs {
		specs[index] = dialSpec{
			localAddr: net.JoinHostPort(bindIP, "0"),
			target:    serverPublicAddress,
		}
	}
	hits, armed, done, counters := launchDialSet(ctx, specs, session)
	var armedCount int
	select {
	case armedCount = <-armed:
	case <-ctx.Done():
		<-done
		return TrialResult{
			Session: session, Elapsed: time.Since(startedAt), Diagnostics: counters.snapshot(),
		}
	}
	if armedCount != mappings {
		cancel()
		<-done
		return TrialResult{
			Session: session, Elapsed: time.Since(startedAt), Diagnostics: counters.snapshot(),
		}
	}
	result := awaitHit(ctx, session, hits, startedAt)
	cancel()
	<-done
	result.Diagnostics = counters.snapshot()
	return result
}

func launchDialSet(
	ctx context.Context,
	specs []dialSpec,
	session string,
) (<-chan dialHit, <-chan int, <-chan struct{}, *attemptCounters) {
	return launchDialSetWithControl(ctx, specs, session, nil)
}

func launchDialSetWithControl(
	ctx context.Context,
	specs []dialSpec,
	session string,
	control dialControlFunc,
) (<-chan dialHit, <-chan int, <-chan struct{}, *attemptCounters) {
	sdkSpecs := make([]burrow.BirthdayDialSpec, len(specs))
	for index, spec := range specs {
		sdkSpecs[index] = burrow.BirthdayDialSpec{
			LocalAddr: spec.localAddr,
			Target:    spec.target,
		}
	}
	dialSet := burrow.LaunchBirthdayDialSet(ctx, sdkSpecs, control)
	hits := make(chan dialHit, 1)
	done := make(chan struct{})
	counters := &attemptCounters{dialSet: dialSet}
	go func() {
		var validations sync.WaitGroup
		validate := func(conn net.Conn) {
			validations.Add(1)
			go func() {
				defer validations.Done()
				defer conn.Close()
				if err := validateConnection(ctx, conn, session); err != nil {
					counters.validationErrors.Add(1)
					return
				}
				counters.validated.Add(1)
				hit := dialHit{localAddr: conn.LocalAddr().String(), remoteAddr: conn.RemoteAddr().String()}
				select {
				case hits <- hit:
				default:
				}
			}()
		}
		for {
			select {
			case conn := <-dialSet.Connections:
				validate(conn)
			case <-dialSet.Done:
				for {
					select {
					case conn := <-dialSet.Connections:
						validate(conn)
					default:
						validations.Wait()
						close(done)
						return
					}
				}
			}
		}
	}()
	return hits, dialSet.Armed, done, counters
}

func awaitHit(ctx context.Context, session string, hits <-chan dialHit, startedAt time.Time) TrialResult {
	select {
	case hit := <-hits:
		return TrialResult{
			Success:    true,
			Session:    session,
			LocalAddr:  hit.localAddr,
			RemoteAddr: hit.remoteAddr,
			Elapsed:    time.Since(startedAt),
		}
	case <-ctx.Done():
		return TrialResult{Session: session, Elapsed: time.Since(startedAt)}
	}
}

func validateConnection(ctx context.Context, conn net.Conn, session string) error {
	payload := []byte("BURROW-BIRTHDAY/1 " + session + "\n")
	deadline := time.Now().Add(time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if !bytes.Equal(reply, payload) {
		return fmt.Errorf("direct connection validation payload mismatch")
	}
	return nil
}
