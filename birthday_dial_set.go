package burrow

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
)

// BirthdayDialSpec describes one TCP candidate socket in a birthday dial set.
type BirthdayDialSpec struct {
	LocalAddr string
	Target    string
}

// BirthdayDialErrorCounts classifies candidate failures observed while the
// shared SDK dial set is running.
type BirthdayDialErrorCounts struct {
	Timeout  int
	Canceled int
	Refused  int
	Resource int
	Other    int
}

// BirthdayDialDiagnostics is a point-in-time snapshot of a shared birthday
// dial set.
type BirthdayDialDiagnostics struct {
	Requested  int
	Armed      int
	Connected  int
	DialErrors BirthdayDialErrorCounts
}

type birthdayDialCounters struct {
	requested int
	armed     atomic.Int64
	connected atomic.Int64
	timeouts  atomic.Int64
	canceled  atomic.Int64
	refused   atomic.Int64
	resource  atomic.Int64
	other     atomic.Int64
}

func (c *birthdayDialCounters) snapshot() BirthdayDialDiagnostics {
	return BirthdayDialDiagnostics{
		Requested: c.requested,
		Armed:     int(c.armed.Load()),
		Connected: int(c.connected.Load()),
		DialErrors: BirthdayDialErrorCounts{
			Timeout:  int(c.timeouts.Load()),
			Canceled: int(c.canceled.Load()),
			Refused:  int(c.refused.Load()),
			Resource: int(c.resource.Load()),
			Other:    int(c.other.Load()),
		},
	}
}

func (c *birthdayDialCounters) recordError(err error) {
	switch {
	case errors.Is(err, context.Canceled):
		c.canceled.Add(1)
	case errors.Is(err, context.DeadlineExceeded):
		c.timeouts.Add(1)
	case errors.Is(err, syscall.ECONNREFUSED):
		c.refused.Add(1)
	case errors.Is(err, syscall.EADDRINUSE),
		errors.Is(err, syscall.EADDRNOTAVAIL),
		errors.Is(err, syscall.EMFILE),
		errors.Is(err, syscall.ENFILE),
		errors.Is(err, syscall.ENOBUFS),
		errors.Is(err, syscall.ENOMEM):
		c.resource.Add(1)
	default:
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			c.timeouts.Add(1)
			return
		}
		c.other.Add(1)
	}
}

// BirthdayDialSet exposes the connection, arming and completion signals from
// the SDK birthday candidate launcher.
type BirthdayDialSet struct {
	Connections <-chan net.Conn
	Armed       <-chan int
	Done        <-chan struct{}
	counters    *birthdayDialCounters
}

// Diagnostics returns a lock-free snapshot of the dial set counters.
func (s *BirthdayDialSet) Diagnostics() BirthdayDialDiagnostics {
	return s.counters.snapshot()
}

// LaunchBirthdayDialSet creates every TCP candidate through one socket arming
// barrier. The Control callback runs after each socket is created and before
// its connect attempt starts.
func LaunchBirthdayDialSet(ctx context.Context, specs []BirthdayDialSpec, control func(network, address string, c syscall.RawConn) error) *BirthdayDialSet {
	hits := make(chan net.Conn, len(specs))
	armed := make(chan int, 1)
	done := make(chan struct{})
	counters := &birthdayDialCounters{requested: len(specs)}
	var armWG sync.WaitGroup
	var workers sync.WaitGroup
	armWG.Add(len(specs))
	workers.Add(len(specs))
	for _, spec := range specs {
		spec := spec
		go func() {
			defer workers.Done()
			var armOnce sync.Once
			markArmed := func(success bool) {
				armOnce.Do(func() {
					if success {
						counters.armed.Add(1)
					}
					armWG.Done()
				})
			}
			tcpNetwork := tcpNetworkForHostPort(spec.LocalAddr)
			dialer := &net.Dialer{
				LocalAddr: mustTCPAddr(spec.LocalAddr),
				Control: func(network, address string, raw syscall.RawConn) error {
					err := socketControl(control)(network, address, raw)
					markArmed(err == nil)
					return err
				},
				KeepAlive: tcpKeepAlivePeriod,
			}
			conn, err := dialer.DialContext(ctx, tcpNetwork, spec.Target)
			markArmed(false)
			if err != nil {
				counters.recordError(err)
				return
			}
			counters.connected.Add(1)
			configureTCPKeepAlive(conn)
			select {
			case hits <- conn:
			case <-ctx.Done():
				_ = conn.Close()
			}
		}()
	}
	go func() {
		armWG.Wait()
		armed <- int(counters.armed.Load())
		close(armed)
	}()
	go func() {
		workers.Wait()
		close(done)
	}()
	return &BirthdayDialSet{Connections: hits, Armed: armed, Done: done, counters: counters}
}
