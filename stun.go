package burrow

// STUN query helpers, shared by the UDP packet API for NAT profiling and
// mapped address queries. Two details are deliberate and were confirmed by
// testing: the RTO derivation in queryUDPMappedAddr and the rolling read
// deadline in stunPacketConn. Rewriting either can reintroduce intermittent
// failures or busy looping.

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/pion/stun/v3"
)

// stunReadSliceTimeout is the rolling deadline for each ReadFrom in
// stunPacketConn.Read. It must be short and rolling rather than a single
// deadline for the whole query: pion/stun's Client runs a goroutine that
// reads in a loop and retries immediately on error, so once a fixed deadline
// passes every Read fails at once and that goroutine busy-loops at 100% CPU
// until Client.Close. A short rolling deadline makes every Read actually
// block. The overall query timeout is enforced by queryUDPMappedAddr through
// the retransmission interval passed to stun.WithRTO.
const stunReadSliceTimeout = 300 * time.Millisecond

// stunPacketConn adapts a bound net.PacketConn to the stun.Connection
// (io.Reader, io.Writer, io.Closer) that pion/stun expects, with server as
// the fixed peer. Write calls WriteTo(server); Read calls ReadFrom and drops
// packets from any other source, since the punching socket may carry other
// UDP traffic such as candidate probes. Close is a no-op: the caller owns the
// socket and the STUN query only borrows it for one exchange.
//
// While borrowed, the socket's read deadline is changed and packets from
// other sources are dropped, so the caller must not send candidate packets
// on the same socket during a query; replies from the punching peer arriving
// in that window would be silently discarded.
type stunPacketConn struct {
	pc     net.PacketConn
	server net.Addr
}

func (c *stunPacketConn) Write(p []byte) (int, error) {
	return c.pc.WriteTo(p, c.server)
}

func (c *stunPacketConn) Read(p []byte) (int, error) {
	for {
		if err := c.pc.SetReadDeadline(time.Now().Add(stunReadSliceTimeout)); err != nil {
			return 0, err
		}
		n, from, err := c.pc.ReadFrom(p)
		if err != nil {
			return n, err
		}
		if from.String() != c.server.String() {
			// Not a response to this query (e.g. a punching probe); drop it.
			continue
		}
		return n, nil
	}
}

func (c *stunPacketConn) Close() error {
	return nil
}

// queryUDPMappedAddr sends one STUN Binding request to server over the
// caller's bound conn and returns the socket's public mapped address.
//
// To query several STUN servers from the same socket, call it repeatedly:
// each call creates and closes a short-lived stun.Client and holds the read
// side of conn only for the duration of the call.
//
// The caller must not send candidate packets on the socket during the query,
// or the peer's replies are silently dropped by stunPacketConn.Read.
func queryUDPMappedAddr(conn net.PacketConn, server string, timeout time.Duration) (*net.UDPAddr, error) {
	// Strip a stun:// or stuns:// scheme: configurations conventionally write
	// "stun://host:port", but ResolveUDPAddr only accepts a bare host:port and
	// fails with "too many colons" otherwise.
	server = strings.TrimPrefix(server, "stun://")
	server = strings.TrimPrefix(server, "stuns://")
	serverAddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, fmt.Errorf("resolve STUN server %q: %w", server, err)
	}

	// pion/stun's Client gives up after 7 linearly increasing retransmissions
	// (deadline = attempt*rto), so the total wait is at most
	// rto*(1+2+...+7) = 28*rto. Derive rto from timeout so the query takes
	// about timeout in total.
	//
	// Do not enforce the timeout with time.AfterFunc(timeout, client.Close):
	// in pion/stun v3.1.6, Client.Close sets c.closed before calling
	// agent.Close, and the resulting handleAgentCallback returns early when
	// it sees c.closed without invoking the pending transaction callback, so
	// client.Do blocks forever (reproduced with a goroutine dump). Rely on
	// the Client's own RTO timeout instead.
	const totalAttempts = 7
	rto := timeout / totalAttempts / (totalAttempts + 1) * 2
	if rto <= 0 {
		rto = 50 * time.Millisecond
	}

	client, err := stun.NewClient(&stunPacketConn{pc: conn, server: serverAddr},
		stun.WithNoConnClose(), stun.WithRTO(rto))
	if err != nil {
		return nil, fmt.Errorf("create STUN client: %w", err)
	}
	defer func() { _ = client.Close() }()

	message := stun.MustBuild(stun.TransactionID, stun.BindingRequest)

	var xorAddr stun.XORMappedAddress
	var doErr error
	if err := client.Do(message, func(event stun.Event) {
		if event.Error != nil {
			doErr = event.Error
			return
		}
		doErr = xorAddr.GetFrom(event.Message)
	}); err != nil {
		return nil, fmt.Errorf("STUN request: %w", err)
	}
	if doErr != nil {
		if errors.Is(doErr, stun.ErrTransactionTimeOut) || errors.Is(doErr, stun.ErrAgentClosed) {
			return nil, fmt.Errorf("STUN request timed out: %w", doErr)
		}
		return nil, fmt.Errorf("parse STUN response: %w", doErr)
	}

	return &net.UDPAddr{IP: xorAddr.IP, Port: xorAddr.Port}, nil
}

// QueryUDPMappedAddr sends one STUN Binding request to server over conn and
// returns the socket's public mapped address. server may be "host:port" or
// "stun://host:port". conn stays open and is only borrowed for the query, so
// the caller must not send other traffic on it meanwhile.
func QueryUDPMappedAddr(conn net.PacketConn, server string, timeout time.Duration) (*net.UDPAddr, error) {
	return queryUDPMappedAddr(conn, server, timeout)
}
