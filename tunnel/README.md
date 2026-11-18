# tunnel: TCP port forwarding over punched connections

Package `tunnel` forwards TCP connections between two hosts behind NAT using
Burrow's TCP hole punching. The **client** listens on a local TCP address and
punches one dedicated hole per accepted connection; the **server** accepts
punched holes and forwards each one to a fixed TCP address.

Both ends are plain pipes. A hole carries the upstream connection's raw bytes:
no custom header, handshake, keepalive or multiplexing, so the traffic on the
wire is exactly the upper-layer protocol's own. The tunnel never parses what it
carries; addressing, encryption and everything else are up to whatever protocol
you run through it.

```
application ──TCP──> 127.0.0.1:1080
                      tunnel client      one punched TCP connection per upstream connection
                ═══════════ hole ═══════════
                      tunnel server      net.Dial("127.0.0.1:29999")
                          │
                          v
                     forward target
```

Both ends meet through a `burrow-signal` server, using the same `room`.

## Running

The tunnel is part of the `burrow` CLI (`make cli` builds `build/burrow`):

```bash
burrow tunnel server -c server.json    # start the server first
burrow tunnel client -c client.json
```

Start the server first. Until a listener is present in the room, the client's
punches fail with "no listener in the target room" and are retried every
second.

Sample configurations are in [`example/`](example/).

## Configuration

Both files are JSON. Durations are strings such as `"250ms"` or `"3s"`.

### Shared fields

| Field | Required | Description |
| --- | --- | --- |
| `signal` | yes | Full signal server URL, e.g. `ws://signal.example.com:9960/signal` |
| `room` | yes | Room name; must match on both ends |
| `bind_interface` | yes | Network interface to punch from, e.g. `en0`, `eth0`, `wlan0` |
| `strategy` | yes | `direct`, `predictive` or `birthday` (`auto` is rejected) |
| `mode` | no | Where signaling parameters go: empty = URL query string, `header` = base64 in a Cookie header |
| `hosts` | no | Static host-to-IPv4/IPv6 map that bypasses DNS; unmapped names still use DNS |
| `log_level` | no | `debug`, `info`, `warn` or `error`; default `info` |
| `max_concurrent_attempts` | no | Maximum in-flight punching attempts (SDK default 2, maximum 16) |
| `predictive.concurrency` | no | Parallel dial sockets for `predictive`; local, may differ per side |
| `predictive.attempt_timeout` | no | Per-attempt timeout for `predictive`; local, may differ per side |

### Client-only fields

| Field | Required | Description |
| --- | --- | --- |
| `listen` | yes | Local TCP listen address, e.g. `127.0.0.1:1080` |
| `trials` | no | Maximum consecutive punch rounds per hole; 0 or unset = 100; negative is rejected |
| `predictive.port_scan_range` | no | Port prediction scan window (negotiated) |
| `birthday.selection` | with `birthday` | `ack` or `fanout`; required when `strategy` is `birthday` |
| `birthday.candidates` | no | Number of candidate ports per round |
| `birthday.min_port`, `birthday.max_port` | no | Candidate port range |
| `birthday.warmup` | no | Warm-up delay before dialing candidates |
| `birthday.attempt_timeout` | no | Timeout of one birthday round |
| `birthday.success_grace` | no | Grace period to collect more successful candidates after the first |
| `birthday.max_success_candidates` | no | Maximum successful candidates kept per round |

Unset `birthday.*` and `predictive.*` values fall back to the SDK defaults.

### Server-only fields

| Field | Required | Description |
| --- | --- | --- |
| `forward` | yes | TCP address every hole is forwarded to, e.g. `127.0.0.1:29999` |
| `stable_side` | with `birthday` | Must be `true` when `strategy` is `birthday` |
| `skip_nat_check` | no | Skip the NAT self-check at startup (see below); default `false` |

### Common mistakes

**Negotiated parameters belong in the client config.** `birthday.*` and
`predictive.port_scan_range` are sent by the client to the server during
signaling. A server config that contains them is rejected at load time rather
than silently ignored, so you never believe a setting took effect when it did
not. `predictive.concurrency` and `predictive.attempt_timeout` are local and
may be set on either side.

**`bind_interface` is required.** When bound to `0.0.0.0`, the kernel may pick
an unroutable source address (for example from a Docker bridge) and send SYNs
out the wrong interface, so punching always fails. The interface is checked at
startup; if it does not exist, the error lists the available interfaces when
they can be enumerated. Sockets are also bound to the device itself
(`SO_BINDTOIFINDEX` on Linux/Android), so a VPN or TUN proxy that owns the
default route cannot capture the signaling connection.

### Server NAT self-check

With `strategy: "birthday"` only, the server detects its NAT type over STUN
before starting (timeout 30s) and **refuses to start behind a symmetric NAT
(NAT4)**. The birthday strategy needs a stable port mapping on the server; a
symmetric NAT allocates a new port for every destination, and with both sides
unstable the per-round hit rate is far too low to connect.

If the detection itself fails (network error, STUN server unreachable,
timeout), the server also refuses to start, because starting without a result
would make the check meaningless. `direct` and `predictive` do not need a
stable listener-side mapping and skip the check.

The probe uses UDP STUN while the tunnel punches TCP, and a NAT may map the two
differently, so the result is an approximation. If you know the detection is
wrong, set `skip_nat_check: true`.

## Behavior

**One hole per connection.** Each accepted client connection is forwarded over
its own punched TCP connection. A hole is never shared between connections and
never returned to the pool once used: the server dials the forward target as
soon as it reads the first byte, so a reused hole would mix a new request into
an old forwarding context.

**Pre-punched pool (client).** On start the client punches holes in the
background and keeps up to 8 idle holes ready. An incoming connection takes an
idle hole immediately. If the pool is empty, the connection is closed right
away instead of waiting for a punch (which can take seconds to tens of
seconds); the upstream application sees a reset and reconnects while the pool
refills. Idle holes are retired after about 60 seconds, well below typical NAT
idle timeouts, and replaced.

**Retries (client).** Punching is serialized: one punch runs at a time. A punch
retries up to `trials` rounds back to back with no backoff, because the birthday
strategy depends on many consecutive rounds. When the peer is not in the room
or the signal server rate-limits the client, it waits a fixed 1 second before
retrying.

**Candidates and zombies (server).** With birthday `fanout`, the server may
accept up to `max_success_candidates` connections in a round while the client
uses only one and closes the rest. A candidate counts as live once its first
byte arrives; a candidate closed by the client is detected by EOF or RST and
dropped. A 10-minute timeout reclaims candidates whose peer vanished without
sending FIN or RST. The timeout is long on purpose: pooled holes legitimately
stay idle until the user opens a connection. `SessionCount` counts only live
holes.

**Unreachable forward target (server).** If the server cannot connect to
`forward`, it closes that hole and does not retry; the client's upstream
connection fails and the application retries.

**No half-close.** When one direction of a connection ends (EOF or error), both
sides are closed immediately; `CloseWrite` semantics are not preserved. A
protocol that half-closes after sending a request and then waits for the
response will see the response truncated. Make sure the protocol you carry
does not rely on half-close.

## Status

`Client.Snapshot()` and `Server.Snapshot()` return a `Status` with the current
state, punched addresses, hole and connection counts, traffic counters and the
latest TCP quality sample. An `Observer` receives a full snapshot on every state
transition. Fast-changing counters such as `ActiveStreams` are not pushed;
poll `Snapshot()` for them.
