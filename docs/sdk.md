# Burrow Go SDK

Package `burrow` (`import "github.com/getkiko/burrow"`) establishes direct TCP
connections between two hosts behind NAT using TCP hole punching, coordinated
through a signal server. It exposes them with standard library semantics:
`Listen` returns a `net.Listener`, and `Dialer.Dial` returns a `net.Conn`.
What runs over the connection is entirely up to you; the signal server never
relays application data.

Every option is listed in [configuration.md](configuration.md).

## Connection delivery

A connection returned by `Accept` or `Dial` starts at the first byte of
application data:

- **direct**, **predictive** and **birthday with fanout selection** return the
  raw punched connection with nothing written on it. The first TCP connection
  that completes is adopted; whether it is usable in both directions is for
  the upper-layer protocol to find out (and retry if it is not). With fanout,
  one Dial may produce several connections on the Listener's `Accept` (up to
  `Birthday.MaxSuccessCandidates`); the upper layer should drop the ones that
  never receive data.
- **birthday with ack selection** first exchanges a fixed-size (73-byte),
  HMAC-authenticated selection frame on the candidate connections, keyed by a
  random per-dial secret, so that exactly one Dial and one Accept agree on the
  same connection. The frame is consumed inside the SDK before the connection
  is returned.

## Signal server

Both sides must reach the same signal server over TCP. The bundled server is
`burrow-signal` (`cmd/burrow-signal`):

```sh
go install github.com/getkiko/burrow/cmd/burrow-signal@latest
burrow-signal -listen :9960,:9962,:9963 -prefix /signal -stat 127.0.0.1:9961
```

| Flag | Default | Description |
|---|---|---|
| `-listen` | `:9960` | Comma-separated listen addresses. |
| `-prefix` | `""` (root) | Fixed path prefix for all endpoints. |
| `-stat` | `""` (off) | Address of a metrics endpoint served at `/stat`. |

Set `BURROW_SIGNAL_SECRET` to fix the key that signs short-lived endpoint
tokens; without it a random key is generated at startup.

Every endpoint is `<prefix>?command=<name>` on any of the listen addresses:

| Command | Protocol | Purpose |
|---|---|---|
| `signal` | WebSocket | Pairing and control messages. One control connection is reused for the lifetime of a Listener or Dialer. |
| `echo` | WebSocket | Reflects the caller's public TCP address. |
| `nat` | WebSocket | TCP mapping lease: reflects the public address and keeps the mapping open. Used for punching and for `auto` NAT probing. |
| `health` | HTTP | Returns 204. |

With `-prefix /signal`, pairing goes to
`ws://signal.example.com:9960/signal?command=signal`, and the SDK side is
configured with `SignalAddr: "ws://signal.example.com:9960/signal"`. With
`Config.Mode = "header"` the query parameters travel in a cookie instead of the
URL. The server also answers on the fixed paths `/signal`, `/echo`, `/probe`,
`/nat` and `/health` for compatibility.

Open the listen ports for **TCP** in every firewall and security group. The
`auto` strategy needs at least three distinct `host:port` probe targets, which
is why the example listens on three ports; probe targets on a second public IP
let the profile also verify cross-IP mapping behavior.

## Listener

`Listen` returns immediately and keeps registering the room in the
background. Each `Accept` returns the next punched connection:

```go
package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/getkiko/burrow"
)

func main() {
	ln, err := burrow.Listen(burrow.Config{
		SignalAddr:    "signal.example.com:9960",
		Room:          "my-room",
		BindInterface: "eth0",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s\n", r.RemoteAddr)
	})
	// http.Serve returns when the listener is closed: burrow.ErrClosed
	// wraps net.ErrClosed.
	log.Fatal(http.Serve(ln, mux))
}
```

Any server that takes a `net.Listener` works the same way, for example
`grpcServer.Serve(ln)`.

## Dialer

Each `Dial` runs a full pairing and punching round and returns an independent
connection. A `Dialer` is safe for concurrent use:

```go
package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/getkiko/burrow"
)

func main() {
	d, err := burrow.NewDialer(burrow.Config{
		SignalAddr:    "signal.example.com:9960",
		Room:          "my-room",
		BindInterface: "en0",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	client := &http.Client{
		Transport: &http.Transport{
			// The address is ignored: every new connection is punched
			// to whoever listens on the room.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return d.Dial(ctx)
			},
		},
		Timeout: 2 * time.Minute,
	}
	resp, err := client.Get("http://peer/")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(os.Stdout, resp.Body)
}
```

The context bounds the whole round (pairing and punching). Each new
connection costs a full punch, so let the transport keep connections alive
rather than opening many in parallel.

Both sides must start within `PairTimeout` (60 s by default) of each other.
A runnable echo server and client are in [examples/](../examples):

```sh
go run ./examples/echo-server -signal signal.example.com:9960 -room demo -iface eth0
go run ./examples/echo-client -signal signal.example.com:9960 -room demo -iface en0 -msg hello
```

## Strategies

`Config.Strategy` selects how the two sides punch. The Dialer owns the
strategy parameters and sends them to the Listener, so `Birthday` and
`Predictive.PortScanRange` are set on the Dialer only (`Listen` rejects them).

| Strategy | NAT situation | What happens |
|---|---|---|
| `direct` | Both NATs keep a stable mapping per local port | Each side binds one local port, learns its public endpoint, and both dial each other while accepting on that port (TCP simultaneous open). |
| `predictive` | Dialer's NAT allocates ports sequentially | The Listener scans a window of ports around the Dialer's reflected port; the Dialer dials the Listener's stable endpoint. See [port-scan-range.md](port-scan-range.md). |
| `birthday` | Dialer's NAT allocates ports unpredictably | The Listener dials many random ports on the Dialer's public IP from its stable port, while the Dialer opens as many connections to the Listener from fresh local ports; a collision between the two sets punches through. |
| `auto` | Unknown or changing networks | The Dialer probes its NAT through `NATProbe.Targets` and the Listener picks direct, predictive or birthday from the result. |

Predictive, birthday and auto assume the Listener is behind a stable-mapping
NAT and sets `StableSide: true` (enforced for birthday and auto).

Listener for a host behind a stable NAT that accepts any strategy:

```go
ln, err := burrow.Listen(burrow.Config{
	SignalAddr:    "signal.example.com:9960",
	Room:          "my-room",
	BindInterface: "eth0",
	Strategy:      burrow.StrategyAuto,
	StableSide:    true,
})
```

Dialer that probes its NAT and chooses automatically:

```go
d, err := burrow.NewDialer(burrow.Config{
	SignalAddr:    "signal.example.com:9960",
	Room:          "my-room",
	BindInterface: "en0",
	Strategy:      burrow.StrategyAuto,
	Birthday: burrow.BirthdayOptions{
		Selection: burrow.CandidateSelectionACK,
	},
	NATProbe: burrow.NATProbeOptions{
		Targets: []string{
			"signal.example.com:9960",
			"signal.example.com:9962",
			"signal.example.com:9963",
		},
	},
})
```

If you already know the Dialer's network, fix the strategy on the Dialer (for
example `burrow.StrategyBirthday` with the same `Birthday` options) and skip
the probe. A Listener with a fixed strategy only accepts Dialers that request
the same strategy or `auto`.

The NAT profile is cached for `NATProbe.CacheTTL` (10 minutes by default) and
measured again when the interface changes, the public IP changes, or two
punches in a row fail.

## Errors

Test errors with `errors.Is`:

| Error | Meaning |
|---|---|
| `ErrPunchFailed` | No usable connection: the NAT does not support TCP simultaneous open, or the peer did not punch in time. Retrying may help. |
| `ErrNoListener` | Nobody is listening on the room. Retrying immediately is pointless. |
| `ErrClosed` | The Listener or Dialer was closed. Wraps `net.ErrClosed`. |
| `ErrUnsupportedNATPair` | `auto` could not classify the NAT, or the Listener is not the stable side. |
| `ErrStrategyMismatch` | The two sides disagree on strategy or parameters. |
| `ErrResourceLimit` | The candidate socket budget is exhausted, or the peer's parameters exceed local limits. |
| `ErrSelectionFailed` | Birthday ack selection did not agree on a connection. |

The Listener reports failures to the Dialer through signaling with the codes
`unsupported_nat_pair`, `strategy_mismatch`, `room_busy` and `punch_failed`.

## Logging

| Field | Receives |
|---|---|
| `InfoLog` | Milestones: pairing complete, punch succeeded (addresses and elapsed time). |
| `DebugLog` | Detailed diagnostics, plus a `tcp_stats` line per established connection every second on Linux and macOS. |
| `StatsLog` | The same per-second statistics as a structured `TCPStats` value, for UIs. |

All three are optional. When `DebugLog` is nil, diagnostics go to the
standard `log` package if the environment variable `BURROW_DEBUG=1` is set, or
after `burrow.SetDebug(true)`. Leave diagnostics off in normal operation: the
retry loops are verbose.

## Concurrency and back-pressure

- `MaxConcurrentAttempts` (default 2, max 16) limits punch sessions in flight
  per Listener or Dialer.
- Each instance has a budget of 512 candidate sockets. A direct attempt takes
  2, predictive `Predictive.Concurrency + 1`, birthday `Birthday.Candidates`.
  Sessions that do not fit wait.
- `AcceptQueue` (default 64) buffers punched connections until `Accept`
  takes them. When it is full, punching blocks, which propagates
  back-pressure to the Dialer.

## Network changes

Call `ResetSignal()` on a Listener or Dialer after the network changes (for
example Wi-Fi to cellular): it drops the reused signaling connection, and the
next operation reconnects over the current network. For hosts whose interface
changes, set `InterfaceName` instead of `BindInterface` so each punch looks up
the current interface.

## Platform notes

- **`SO_REUSEPORT` is required.** Address reflection, listening and dialing
  must share one local port, otherwise the NAT allocates a new public port and
  the exchanged address is useless. Hole punching works on Linux, macOS,
  FreeBSD, NetBSD, OpenBSD and DragonFly BSD (`reuse_unix.go`). On other
  platforms (including Windows) the package compiles, but punching fails with
  `ErrUnsupportedPlatform`. That variable is only declared on unsupported
  platforms (`reuse_unsupported.go`), so portable code should not reference it
  directly.
- **Bind to the physical interface.** Set `BindInterface` explicitly. With
  Docker bridges or other virtual interfaces the kernel may choose an
  unroutable source address. When a TUN device owns the default route (a VPN
  app, a global proxy), also pass a `Control` function that binds sockets to
  the device (`SO_BINDTODEVICE` on Linux and Android, `IP_BOUND_IF` on
  macOS); otherwise the signaling connection is captured by the TUN device,
  the server sees the wrong public IP, and every punch targets the wrong
  address.
- **Android.** Unprivileged apps may not read the routing table over
  netlink. Provide `InterfaceAddr` from the platform's network APIs, or rely
  on the built-in ioctl lookup for IPv4. Gomobile bindings live in
  [mobile/](../mobile).
- **TCP statistics** (`StatsLog`, `tcp_stats`) are available on Linux and
  macOS only.

## How a punch works

1. Pick the interface and bind a local port `P`. The same port is used for
   the whole round.
2. Start listening on `P` immediately, before knowing the peer. A SYN that
   arrives before the listener exists would be answered with RST.
3. Open a mapping lease to the signal server from `P` to learn and hold the
   public **TCP** endpoint (UDP mappings are separate and would be wrong).
4. Exchange public endpoints through signaling.
5. Both sides dial each other while accepting, and claim the connection
   whose remote address matches the peer. Dialing to one peer is serial with
   rapid retries, because a second in-flight connection with the same 4-tuple
   fails with `EADDRINUSE`.

Pairing is per session: every Dial carries a unique session ID and the signal
server delivers it to the room's Listener only. There is no anonymous slot to
race for, so two Dialers can never be paired with each other, and a
connection can never be handed to the wrong Dial. The SDK also rejects a peer
endpoint equal to its own.

## Limitations

- Two hosts behind the same NAT may not reach each other (no hairpinning);
  put them behind different public addresses.
- The Listener must be the stable-mapping side for predictive, birthday and
  auto. If both NATs are destination-dependent, none of the strategies apply.
- TCP only for `Listen`/`Dialer`; the UDP packet API below is experimental.

## Experimental UDP packet API

`ListenPacket` and `NewPacketDialer` (configured with `PacketConfig`) are a
UDP counterpart intended for upper-layer protocols such as QUIC. They support
the `direct` and `predictive` strategies and require at least one STUN server.

Instead of a single connection, `DialPacket(ctx)` and `AcceptPacket(ctx)`
return a `*PacketAttempt` holding the prepared candidate paths: bound UDP
sockets with their STUN mapping and the remote addresses to try. Selecting a
path is the caller's job: send real handshake packets on the candidates, then
call `Commit(path, peer)` to keep the winning socket as a `net.PacketConn`
and close the others. Always call `Close` on the attempt; after `Commit` it
also closes the winning socket, so call it only when you are done with that
connection.

```go
attempt, err := d.DialPacket(ctx)
if err != nil {
	return err
}
defer attempt.Close()

// Probe attempt.Paths[i].Conn against attempt.Paths[i].Remotes with your
// protocol's handshake, then keep the path that authenticated.
pc, err := attempt.Commit(winner, remote)
```

`PacketConfig.SignProof`/`VerifyProof` optionally authenticate the
signaling messages; `NewProofSigner(password)` and
`NewProofVerifier(password)` provide an HMAC-SHA256 implementation. This API
may change.
