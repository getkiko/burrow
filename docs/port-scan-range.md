# Choosing a predictive port scan range

The `predictive` strategy handles a NAT that allocates public TCP ports
*sequentially*: every new destination gets a new public port, usually a little
above the previous one. The port the signal server reflects (`P`) is therefore
not the port the peer will actually use when it punches (`P + Δ`). Predictive
covers that drift by scanning a window of ports around `P`.

This document explains who scans, how the window is computed, and how to size
it. Field names are given for the Go SDK (`burrow.Config`) and for the tunnel
JSON config (`burrow tunnel`).

## Who scans what

Predictive assumes a fixed topology:

- The **Listener** sits behind a stable-mapping NAT (public IP, full cone,
  restricted cone, port-restricted cone, or any NAT whose mapping does not
  depend on the destination) and sets `StableSide: true`.
- The **Dialer** sits behind the sequential NAT.

During a punch the Dialer dials the Listener's stable public endpoint exactly,
retrying every 200 ms, while the Listener scans a window of ports on the
Dialer's public IP centered on the Dialer's reflected port. The first TCP
connection that completes wins and cancels the rest of the round.

If the sequential NAT is on the host that runs the service, swap the roles:
run the Listener on the stable host and the Dialer on the other one. Burrow
has no mode in which both sides scan; if both NATs are destination-dependent,
neither predictive nor birthday applies.

## Where the range is configured

| Setting | Go SDK | Tunnel JSON | Set on | Default |
|---|---|---|---|---|
| Window width `N` | `Predictive.PortScanRange` | `predictive.port_scan_range` | Dialer / client only | `512` |
| Parallel dials | `Predictive.Concurrency` | `predictive.concurrency` | either side (local) | `64` |
| Scan duration | `Predictive.AttemptTimeout` | `predictive.attempt_timeout` | either side (local) | `2s` |

The window width is negotiated: the Dialer sends it to the Listener in the DIAL
request, and `burrow.Listen` rejects a config that sets it (as does the tunnel
server config loader). Concurrency and attempt timeout are local resource
limits; they matter on the Listener, because the Listener is the side that
scans.

The legacy field `Config.PortScanRange` is still accepted on the Dialer. When
`Predictive.PortScanRange` is zero it is used as the window width, and when
`Strategy` is empty a non-zero `PortScanRange` selects predictive. New code
should set `Strategy` and `Predictive.PortScanRange` explicitly.

## The window

For a reflected port `P` and range `N` the Listener scans:

```text
start = P - N/2
end   = start + N
```

The window is shifted (not truncated) when it would cross port 1 or 65535, so
it always covers `N + 1` ports. It is centered rather than upward-only because
other connections may consume ports between the reflection and the punch, and
the target can land on either side of `P`.

Each round dials every port in the window with up to `Concurrency` parallel
connection attempts and a 300 ms per-dial timeout, waits 100 ms, and starts
again until a connection completes or `AttemptTimeout` expires.

## Sizing the window

Start small and grow only as far as measurements require.

| `N` | Typical use | Ports per round |
|---:|---|---:|
| `64` | Small, steady increments | 65 |
| `128` | Common sequential allocation | 129 |
| `256` | Larger or noisier increments | 257 |
| `512` (default) | Increment pattern unknown | 513 |

To pick a value, record over several punches:

```text
Δ = port that actually connected - reflected port
```

Debug logs (`BURROW_DEBUG=1` or a `DebugLog` callback) show the scanned window
and the winning connection. Set `N` to roughly twice the high percentile of
`|Δ|` (the window extends `N/2` on each side) plus a 20-50% margin. For example,
if almost every `|Δ|` is below 40, `N = 128` is enough.

`burrow tcpnat` measures how your NAT maps successive TCP connections to
different destinations, which tells you whether the NAT is stable, sequential
or dispersed before you pick a strategy:

```sh
burrow tcpnat -server signal.example.com:9960 -ports 9960,9962,9963 -rounds 5
```

If the measured ports are scattered rather than drifting by a small amount, a
wider window will not help; use the `birthday` strategy instead.

## How `auto` uses the range

With `Strategy: auto` the Dialer probes its own NAT through the targets in
`NATProbe.Targets` and classifies it. For each sample it predicts the next
port from the previous step and counts a hit when the prediction lands within
`max(N/4, 1)` ports of the observed one:

- every sample shows the same port on every target: **stable**, use `direct`;
- at least 8 trials and at least 7/8 of them hit: **sequential**, use
  `predictive`;
- at least 8 trials and at most half of them hit: **dispersed**, use
  `birthday`;
- anything else: **unknown**, and the Dial fails with `ErrUnsupportedNATPair`.

A larger window therefore also makes `auto` more willing to classify a NAT as
sequential.

## Resource cost

Each predictive attempt reserves `Concurrency + 1` sockets from the instance's
candidate socket budget of 512, and up to `MaxConcurrentAttempts` (default 2)
attempts run at once. A Listener scanning a 512-port window therefore sends
513 SYNs per round per attempt, and two concurrent attempts double that.

Bursts of short-lived SYNs fill the NAT/conntrack tables of the local host and
of the home router. UDP and DNS are usually the first to suffer, showing up as
packet loss or timeouts. To keep this under control:

- keep the window as small as your measurements allow;
- lower `Concurrency` (16 or 32) on constrained devices;
- keep `MaxConcurrentAttempts` low, and remember that every new connection an
  upper layer opens (for example a connection pool or a multiplexer opening a
  new physical connection, or a protocol retrying its handshake) triggers a
  full punch with a full scan.

## UDP packet API

The experimental packet API (`PacketConfig`) uses the same idea for UDP: the
stable side probes a window of ports around the peer's STUN mapping, expanding
outward from the center. Its default range is also 512 and the maximum is
4096 ports.
