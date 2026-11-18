# Burrow

Burrow opens direct TCP connections between two hosts that are both behind NAT,
and hands them to you as an ordinary `net.Listener` and `net.Conn`.

A small public **signal server** introduces the two peers; after that, every byte
flows over a TCP connection established by simultaneous open (TCP hole punching).
Nothing is relayed.

## Features

- **Standard Go interfaces.** `burrow.Listen` returns a `net.Listener` you can hand
  to `http.Serve` or a gRPC server; `Dialer.Dial` returns a `net.Conn`.
- **Several punching strategies** for different NAT types, plus `auto`, which
  measures the dialer's NAT and picks one.
- **Clean connections.** The first byte you read is the first byte the peer wrote.
  Burrow never adds framing on the data path (the optional `ack` selection mode
  exchanges a fixed-size authenticated prefix and consumes it before returning).
- **Built for real networks:** binding to a physical interface when a VPN or TUN
  device owns the default route, IPv4 or IPv6, custom DNS resolution for the signal
  server, per-connection TCP statistics, back-pressure and resource budgets.
- **Batteries included:** a self-hostable signal server with a one-line installer,
  a CLI with NAT diagnostics and a web console, a TCP port-forwarding tunnel, and
  an Android client.

## How it works

```
   host A (listener)                 signal server                 host B (dialer)
        |  LISTEN room ---------------->  |                               |
        |                                 |  <------------- DIAL room     |
        |  <----------- INCOMING          |                               |
        |  echo: learn public ip:port --> |  <-- echo: learn public ip:port
        |  READY (endpoint) ------------> |  -------------> PEER (endpoint)
        |                                                                 |
        |  <==========  simultaneous TCP open, same local ports  ========>|
```

1. **Signaling.** The listener keeps a WebSocket open to the signal server and
   registers a *room*. The dialer joins the same room with a fresh session ID.
2. **Address discovery.** Each side opens a TCP connection to the server's echo
   endpoint *from the local port it will punch with*, so the server reports the
   public `ip:port` the NAT assigned to that port (STUN, but for TCP). The mapping
   is held open while the attempt runs.
3. **Punching.** Both sides exchange endpoints through the server and dial each
   other from the same local port (`SO_REUSEADDR`/`SO_REUSEPORT`) while also
   accepting on it. Whichever TCP handshake completes first wins.

How each side dials depends on the strategy:

| Strategy | Use it when | What happens |
|---|---|---|
| `direct` | Both NATs keep the same public port for every destination (endpoint-independent mapping, "cone" NATs). | Each side dials the exact endpoint the other reported. |
| `predictive` | The dialer's NAT allocates ports sequentially. | The listener (which must be on the stable side) dials a window of `Predictive.PortScanRange` ports (default 512) around the dialer's observed port. |
| `birthday` | The dialer's NAT picks ports randomly (symmetric / carrier-grade NAT). | The stable side pre-opens `Birthday.Candidates` (default 256) connections to random ports while the other side opens as many connections to the stable endpoint; a collision yields a connection, as in the birthday paradox. |
| `auto` | You do not know the dialer's NAT. | The dialer samples its NAT mapping behaviour against at least three echo endpoints (`NATProbe.Targets`) and picks `direct`, `predictive` or `birthday`. |

Negotiated parameters (strategy, scan range, birthday settings) are chosen by the
dialer and sent to the listener in the `DIAL` message; both sides verify a
fingerprint of them, so mismatched configurations fail fast instead of punching
blindly. `predictive`, `birthday` and `auto` assume the listener is the side with a
stable mapping; `birthday` and `auto` require the listener to declare it with
`StableSide: true`.

Burrow also contains an experimental UDP API (`ListenPacket`, `NewPacketDialer`)
that punches UDP paths for use by an upper-layer protocol such as QUIC; it supports
the `direct` and `predictive` strategies.

## Install

```sh
go get github.com/getkiko/burrow
```

Burrow needs Go 1.25 or later. TCP hole punching relies on `SO_REUSEPORT` and works
on Linux (including Android), macOS and the BSDs; on other platforms `Listen` and
`Dial` return an error.

The CLI and the signal server:

```sh
go install github.com/getkiko/burrow/cmd/burrow@latest
go install github.com/getkiko/burrow/cmd/burrow-signal@latest
```

Prebuilt binaries are attached to each
[GitHub release](https://github.com/getkiko/burrow/releases).

## SDK usage

Both peers need the same signal server address and the same room name.

Listener:

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
		BindInterface: "eth0", // recommended: the physical interface to punch from
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	// ln is a net.Listener: every Accept returns a new punched connection.
	log.Fatal(http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s\n", r.Host)
	})))
}
```

Dialer:

```go
package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/getkiko/burrow"
)

func main() {
	d, err := burrow.NewDialer(burrow.Config{
		SignalAddr: "signal.example.com:9960",
		Room:       "my-room",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	// Each Dial runs one punching round and returns an independent net.Conn.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.Dial(ctx)
		},
	}}
	resp, err := client.Get("http://peer/")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(os.Stdout, resp.Body)
}
```

Choosing a strategy on the dialer (the listener adopts it from the signal message):

```go
d, err := burrow.NewDialer(burrow.Config{
	SignalAddr: "signal.example.com:9960",
	Room:       "my-room",
	Strategy:   burrow.StrategyAuto,
	Birthday: burrow.BirthdayOptions{
		Selection: burrow.CandidateSelectionACK, // required for birthday and auto
	},
	// auto measures the NAT against at least three distinct echo endpoints,
	// e.g. one signal server listening on several ports.
	NATProbe: burrow.NATProbeOptions{
		Targets: []string{
			"signal.example.com:9960",
			"signal.example.com:9962",
			"signal.example.com:9963",
		},
	},
})

ln, err := burrow.Listen(burrow.Config{
	SignalAddr: "signal.example.com:9960",
	Room:       "my-room",
	Strategy:   burrow.StrategyAuto,
	StableSide: true, // this host's NAT keeps a stable mapping
})
```

Errors can be inspected with `errors.Is`: `ErrPunchFailed`, `ErrNoListener`,
`ErrUnsupportedNATPair`, `ErrStrategyMismatch`, `ErrResourceLimit`, `ErrClosed`.
Set `InfoLog`/`DebugLog` to route logs into your logger, or set `BURROW_DEBUG=1`
for diagnostic output on stderr. See [docs/sdk.md](docs/sdk.md) and
[docs/configuration.md](docs/configuration.md) for every option, and
[examples/](examples) for a runnable echo server and client.

## CLI

`burrow` bundles a port-forwarding tunnel, a punching demo and NAT diagnostics:

```sh
# Forward local port 1080 to 127.0.0.1:29999 on a host behind another NAT.
burrow tunnel server -c server.json     # on the host that runs the service
burrow tunnel client -c client.json     # on the host that wants to reach it

# One-shot TCP punch with an HTTP request on top (and an optional throughput test).
# -iface is optional; without it the kernel routing table picks the interface.
burrow punch server -signal signal.example.com:9960 -room demo [-iface eth0]
burrow punch client -signal signal.example.com:9960 -room demo [-iface en0] [-bench -size 100MB]

# Diagnose your NAT.
burrow tcpnat -server signal.example.com:9960 -ports 9960,9962,9963 -rounds 5
burrow stun                              # UDP NAT type via STUN

# The same tools in a browser: http://127.0.0.1:18083/
# It must listen on a loopback address (-listen 127.0.0.1:PORT, [::1]:PORT or
# localhost:PORT) and only accepts requests addressed to localhost or a loopback
# IP. It has no authentication; for remote use, forward the port over SSH.
burrow api
```

Tunnel configuration is JSON; see [tunnel/README.md](tunnel/README.md) and the samples
in [tunnel/example/](tunnel/example). `burrow birthday server|client` runs the
birthday-collision experiment described in
[internal/birthdaylab/README.md](internal/birthdaylab/README.md).

## Signal server

The signal server only introduces peers and echoes public addresses; it never sees
application data. It needs a public IP and inbound TCP on the ports it listens on.
Clients must reach those ports directly: the address echo reports the TCP source
address the server sees, and echo tokens are bound to that address, so the server
cannot run behind a reverse proxy, a load balancer or any other L7 or TCP-terminating
front end.

One-line install on Linux (systemd), from the latest GitHub release:

```sh
curl -fsSL https://raw.githubusercontent.com/getkiko/burrow/main/signal-deploy/install.sh | sudo bash
```

Or run it yourself:

```sh
burrow-signal -listen :9960,:9962,:9963 -stat 127.0.0.1:9961
```

- `-listen` takes several ports. Every port serves the same room registry; the extra
  ports let `auto` and `burrow tcpnat` observe how the NAT maps one local port to
  different destinations.
- `-prefix /some/path` changes the HTTP path of the entry point
  (`<prefix>?command=signal|echo|nat|health`); it only renames the path and does not
  make the server proxy-friendly. Clients then use
  `signal.example.com:9960/some/path` as `SignalAddr`. The fixed paths `/signal`,
  `/echo`, `/probe`, `/nat` and `/health` are always served as well.
- `BURROW_SIGNAL_SECRET` sets the key that signs echo tokens; set it to a random value
  so tokens survive restarts and cannot be forged.
- `-stat` exposes counters at `http://<addr>/stat`; bind it to localhost.
- Abuse limits: LISTEN and DIAL are rate limited per source, where a source is an
  IPv4 address or an IPv6 /64. `-max-signal-per-ip` (default 256) caps concurrent
  signaling connections per source, `-max-nat-per-ip` (default 64) caps concurrent
  `nat` mapping probes per source, and `-max-rooms-per-conn` (default 32) caps the
  rooms one connection may listen on or dial into; `0` disables a cap.
  `-idle-timeout` (default 60s) closes signaling connections that send nothing;
  listeners renew every 5 seconds, so only idle clients are affected and they
  reconnect when needed.

See [signal-deploy/README.md](signal-deploy/README.md) for the installer's options.

## Android

[android/](android) is a Kotlin / Jetpack Compose client that runs the tunnel on a
phone, with a NAT diagnostics screen and live statistics. It uses the gomobile
bindings in [mobile/](mobile):

```sh
make aar-install   # once: install gomobile and gobind
make apk           # builds build/tunnel.aar, copies it into the app and assembles a debug APK
```

See [android/README.md](android/README.md).

## Limitations

- **TCP only** for the `net.Listener` / `net.Conn` API; the UDP packet API is
  experimental and supports only `direct` and `predictive`.
- **Not every NAT pair can be punched.** Two randomly-mapping NATs cannot be
  traversed by any strategy here; `predictive`, `birthday` and `auto` need one side
  with a stable mapping. There is no relay fallback; `Dial` returns an error instead.
- **No encryption or authentication of the data path.** Run TLS (or another
  authenticated protocol) over the connection.
- **Room names are not secrets, and the signal server does not authenticate
  listeners.** Anyone who knows the room can dial your listener, and anyone who knows
  it can also register as the listener of that room: the room follows whichever
  listener renewed last (listeners renew every few seconds), so an impostor can
  receive some of your dialers' attempts. Authenticate both ends above Burrow, for example with
  mutual TLS, and treat a failed handshake as a failed attempt. With the UDP packet
  API, set `SignProof`/`VerifyProof` (see `NewProofSigner`): a peer without the shared
  password cannot produce a valid proof, so an impostor turns into a failed attempt
  instead of a connection. The signal server never holds that password, so it
  cannot itself decide which listener is genuine.
- Requires `SO_REUSEPORT` (Linux, Android, macOS, BSD). Windows is not supported.
- Firewalls that drop unsolicited inbound SYNs regardless of NAT state, and middleboxes
  that rewrite TCP sequence numbers, may defeat simultaneous open.

## License

[MIT](LICENSE)
