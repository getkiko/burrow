package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/getkiko/burrow"
	"github.com/getkiko/burrow/internal/netiface"
)

// Client listens on a local TCP address and forwards each accepted connection
// over its own hole (one-to-one).
type Client struct {
	listener net.Listener
	tracker  *statusTracker
	logger   *Logger
	sessions *sessionManager

	// activeConns tracks every local TCP connection currently in handle(), so
	// Close can close them.
	//
	// In relayDeferReverse the conn->stream direction blocks in conn.Read and
	// only unblocks when conn is closed or the stream signals closeNotifier.
	// If the upper protocol is server-speaks-first (the local conn waits for
	// a greeting and never writes or disconnects) and the stream (lazyConn)
	// never punched a hole (so there is no asynchronous "tunnel down"
	// signal), the only way to finish that connection is to close conn
	// explicitly. Without this, Close would close only the listener and the
	// punched holes, wg.Wait would wait forever for those handle()
	// goroutines, and Close itself would deadlock.
	//
	// registerMu makes "check whether closed" and "Store into activeConns"
	// atomic. acceptLoop and Close run concurrently; merely registering
	// before wg.Go still lets "Accept returns conn" interleave with "Close
	// runs Range" so that Store lands after Range, the conn is missed, and
	// wg.Wait hangs. Close therefore sets closed and runs Range under the
	// lock, and registerConn checks closed under the same lock before
	// Store; if closed is set it does not Store and the caller closes the
	// conn itself. Every conn is either closed by Close's Range or closes
	// itself at registration; nothing slips through (the same technique as
	// registerConn/registerSession in server.go).
	registerMu  sync.Mutex
	closed      bool
	activeConns sync.Map // net.Conn -> struct{}

	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

// registerConn records conn in activeConns. acceptLoop calls it after Accept
// and before scheduling handle. It returns false if Close has already run (or
// is running) its Range; the caller must then close conn itself and not
// schedule handle, because Close will never Range again.
func (c *Client) registerConn(conn net.Conn) bool {
	c.registerMu.Lock()
	defer c.registerMu.Unlock()
	if c.closed {
		return false
	}
	c.activeConns.Store(conn, struct{}{})
	return true
}

// NewClient builds a Client from cfg. observer may be nil.
//
// All fallible setup (interface check, Dialer creation, listening) happens
// here, so configuration errors surface at startup instead of as unexplained
// connection failures later.
func NewClient(cfg ClientConfig, observer Observer) (*Client, error) {
	if err := verifyInterface(cfg.BindInterface); err != nil {
		return nil, err
	}
	logger := newLoggerWithSink(cfg.LogLevel, cfg.LogSink)
	tracker := newStatusTracker(cfg.Room, observer)

	burrowCfg := cfg.ToBurrowConfig()
	burrowCfg.DebugLog = logger.SDKDebugFunc()
	burrowCfg.InfoLog = logger.SDKInfoFunc()
	burrowCfg.StatsLog = tracker.setStats
	if len(cfg.Hosts) > 0 {
		burrowCfg.ResolveIP = staticHostsResolver(cfg.Hosts)
	}

	dialer, err := burrow.NewDialer(burrowCfg)
	if err != nil {
		return nil, fmt.Errorf("create dialer: %w", err)
	}

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		dialer.Close()
		return nil, fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}

	return &Client{
		listener: listener,
		tracker:  tracker,
		logger:   logger,
		sessions: newSessionManager(dialer, tracker, logger, cfg.Trials),
		done:     make(chan struct{}),
	}, nil
}

// Start begins accepting TCP connections. It does not block.
//
// It also starts background pre-punching: a few holes are punched as soon as
// the tunnel is up, and incoming connections take one from the pool, saving
// the whole punching delay (seconds to tens of seconds with birthday).
func (c *Client) Start() error {
	c.logger.Infof("TCP listening on %s", c.listener.Addr())
	c.wg.Go(c.acceptLoop)
	c.sessions.startPool(defaultPoolSize)
	return nil
}

func (c *Client) acceptLoop() {
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			select {
			case <-c.done:
			default:
				if !isNormalClose(err) {
					c.logger.Errorf("accept: %v", err)
				}
			}
			return
		}
		// Register before wg.Go schedules handle; registerConn and Close
		// share registerMu so registration cannot interleave with Close's
		// Range (see the registerMu field comment). If Close already ran,
		// close the conn here instead of starting a goroutine that would be
		// torn down immediately.
		if !c.registerConn(conn) {
			conn.Close()
			continue
		}
		c.wg.Go(func() { c.handle(conn) })
	}
}

// handle forwards one TCP connection over a dedicated hole in both directions
// without parsing it. The upper protocol is the user's business; the tunnel
// only moves bytes.
//
// Each upstream connection gets its own hole, carrying a raw byte stream with
// no multiplexing header, handshake or keepalive. This is a hard requirement
// on the tunnel's traffic pattern.
func (c *Client) handle(conn net.Conn) {
	defer conn.Close()
	// acceptLoop registered conn in activeConns before scheduling this
	// goroutine; remove it on exit.
	defer c.activeConns.Delete(conn)

	// Use only holes that are already punched: if the pool is empty, close
	// the upstream connection instead of punching inline.
	//
	// Punching is serial and may take hundreds of rounds (seconds to tens of
	// seconds). Holding the upstream connection meanwhile looks like a
	// browser spinning until it times out, and the upstream client's retries
	// are stuck behind it. Closing right away lets the upstream see a reset
	// and reconnect while the background refill fills the pool, so the next
	// attempt connects immediately.
	//
	// lazyConn is still used as the wrapper because it handles recycling and
	// CloseNotify; in pooledOnly mode ensure() only checks the pool and never
	// punches.
	stream := newPooledConn(c.sessions)
	defer stream.Close()

	if _, err := stream.ensure(); err != nil {
		// An empty pool is expected (just started, or holes just used up),
		// not a fault: log at info and say what happens next so the user
		// does not think the tunnel is broken.
		if errors.Is(err, errNoIdleHole) {
			c.logger.Infof("no idle hole, closed %s; the pool is refilling in the background, retry shortly",
				conn.RemoteAddr())
			return
		}
		c.logger.Warnf("prepare tunnel for %s: %v", conn.RemoteAddr(), err)
		return
	}

	c.tracker.streamOpened()
	defer c.tracker.streamClosed()

	// Logged at info, mirroring the server's forwarding log. With one line on
	// each side you can tell where data stalls when the hole is up but pages
	// do not load: did the client get the upstream connection, how many bytes
	// went out, did the server receive them?
	c.logger.Infof("local connection accepted from %s", conn.RemoteAddr())
	// Count bytes as they flow, as on the server; adding them up at the end
	// would show zero traffic and speed for the lifetime of a long
	// connection. fromClient goes out through the tunnel (upload, tx);
	// fromTunnel comes back from it (download, rx).
	fromClient, fromTunnel, _ := relayDeferReverse(
		conn, stream,
		func(n uint64) { c.tracker.addAppTraffic(0, n) },
		func(n uint64) { c.tracker.addAppTraffic(n, 0) },
	)
	c.logger.Infof("local connection closed up=%dB down=%dB", fromClient, fromTunnel)
}

// Snapshot returns a snapshot of the current status.
func (c *Client) Snapshot() Status {
	return c.tracker.Snapshot()
}

// Close shuts the Client down. It is safe to call more than once.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.listener.Close()
		c.sessions.Close()

		// Setting closed and running Range under registerMu is essential:
		// registerConn uses the same lock, so once it is released no conn can
		// be stored without this Range seeing it (see the registerMu field
		// comment).
		c.registerMu.Lock()
		c.closed = true
		// Close every local connection still in handle(). sessions.Close()
		// only closes punched holes and does nothing for connections that
		// never punched and are waiting for the peer to speak first; those
		// conns must be closed directly or wg.Wait() waits forever.
		c.activeConns.Range(func(key, _ any) bool {
			key.(net.Conn).Close()
			return true
		})
		c.registerMu.Unlock()
	})
	c.wg.Wait()
	return nil
}

// verifyInterface checks that the named interface exists and, if it does not,
// tries to list the available interfaces to help troubleshooting.
//
// With 0.0.0.0 the kernel may pick an unroutable source address from a
// virtual interface such as a Docker bridge, sending SYNs out the wrong
// interface so punching always fails; the interface must therefore be set
// explicitly and verified up front.
//
// The existence check uses netiface.IndexByName, which on Linux/Android
// prefers ioctl (SIOCGIFINDEX) over netlink to avoid the SELinux restriction
// on unprivileged Android processes (netlinkrib: permission denied).
//
// Listing available interfaces uses netiface.ListNames(), which has no ioctl
// alternative (ioctl looks up by name but cannot enumerate) and will likely
// fail on Android. A listing failure deliberately does not pollute the main
// error: on success the candidates are appended, on failure only "does not
// exist" is reported rather than a confusing double error.
func verifyInterface(name string) error {
	if _, err := netiface.IndexByName(name); err == nil {
		return nil
	}
	available, listErr := netiface.ListNames()
	if listErr != nil {
		return fmt.Errorf("network interface %q does not exist", name)
	}
	return fmt.Errorf("network interface %q does not exist, available: %v", name, available)
}

// staticHostsResolver resolves host names from the static hosts map in the
// config, bypassing system DNS; unmapped names fall back to DNS (IPv4).
func staticHostsResolver(hosts map[string]string) burrow.ResolveIPFunc {
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		mapped, ok := hosts[host]
		if !ok {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		}
		addr, err := netip.ParseAddr(mapped)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q for %q in hosts: %w", mapped, host, err)
		}
		return []netip.Addr{addr}, nil
	}
}
