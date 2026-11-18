# Configuration reference

This is the reference for every option of the Go SDK (`burrow.Config`,
`burrow.PacketConfig`) and of the tunnel JSON files used by
`burrow tunnel client|server -c <file>`. For how the pieces fit together see
[sdk.md](sdk.md); for sizing the predictive window see
[port-scan-range.md](port-scan-range.md).

Defaults are applied by `burrow.Listen`, `burrow.NewDialer`,
`burrow.ListenPacket` and `burrow.NewPacketDialer`. A zero or negative value
means "use the default" unless stated otherwise.

## Who decides what

The Dialer owns the strategy parameters. It sends them to the Listener in the
DIAL request together with a fingerprint, and the Listener adopts them for
that session. As a consequence:

| Parameter | Dialer | Listener |
|---|---|---|
| `Birthday.*` | set here | rejected by `Listen` |
| `Predictive.PortScanRange`, `PortScanRange` | set here | rejected by `Listen` |
| `Predictive.Concurrency`, `Predictive.AttemptTimeout` | local | local |
| `StableSide` | ignored | set here |
| `NATProbe.*` | used by `auto` | ignored |
| `Strategy` | set here | set here (see [Strategy matching](#strategy-matching)) |
| everything else | local | local |

`SignalAddr` and `Room` must name the same signal server and room on both
sides.

## `burrow.Config`

### Signaling

| Field | Type | Default | Description |
|---|---|---|---|
| `SignalAddr` | `string` | required | Signal server address. Either `host:port` (plain `ws://`) or a URL `ws://host[:port][/prefix]` / `wss://host[:port][/prefix]`. The URL path is kept as the endpoint prefix; a URL without a port uses 80 for `ws` and 443 for `wss`. |
| `Room` | `string` | required | Room name: non-blank, at most 128 bytes (the signal server's limit). Both sides must use the same value. |
| `Mode` | `string` | `""` | How signaling parameters are sent. `""` puts them in the query string (`?command=nat&room=...&session=...`); `"header"` base64-encodes them into a cookie so `command`, `room` and `session` do not appear in the URL. The signal server accepts both. |
| `ResolveIP` | `ResolveIPFunc` | system resolver | Resolves the signal server's host name. The URL keeps the host name, so HTTP `Host` and TLS SNI are unchanged. Addresses that match `AddressFamily` are tried in order. |

### Network

| Field | Type | Default | Description |
|---|---|---|---|
| `AddressFamily` | `string` | `"ipv4"` | `"ipv4"` or `"ipv6"`. Address reflection, signaling, listening and dialing all use this family, so one punch reuses one local IP and port. Any other value is an error. |
| `BindInterface` | `string` | `""` | Interface the punching sockets bind to, such as `en0` or `eth0`. Strongly recommended: with an empty value the kernel picks the source address, and on hosts with Docker bridges or other virtual interfaces it may pick an unroutable one. |
| `InterfaceName` | `func() string` | nil | Called at the start of each punch to get the current interface name, for hosts whose default interface changes. `BindInterface` takes precedence when set. |
| `InterfaceAddr` | `func(name string) string` | nil | Returns the address of the named interface in the configured family, or `""` if unknown. Lets a host that already knows its interfaces (for example through a mobile VPN API) avoid netlink lookups that a sandbox may deny. Without it IPv4 uses an ioctl lookup with a fallback to `net.InterfaceByName`, and IPv6 enumerates global addresses through `net.InterfaceByName`. |
| `Control` | `func(network, address string, c syscall.RawConn) error` | nil | Extra socket setup, run after the internal `SO_REUSEADDR`/`SO_REUSEPORT` setup on every punching socket (reflection, listen, dial, port allocation). Typical use is binding to a device (`SO_BINDTODEVICE`, `IP_BOUND_IF`) so traffic bypasses a TUN device that owns the default route. Same signature as `net.Dialer.Control`. |

### Timeouts and capacity

| Field | Type | Default | Limits | Description |
|---|---|---|---|---|
| `PairTimeout` | `time.Duration` | `60s` | | Maximum time to wait for the peer to show up in the room. |
| `PunchTimeout` | `time.Duration` | `45s` | | Maximum time for one direct punch, and for the Dialer's side of a predictive punch. The Listener's predictive scan uses `Predictive.AttemptTimeout`; birthday uses its own timeouts. |
| `AcceptQueue` | `int` | `64` | | Listener: punched connections buffered until `Accept` takes them. When full, punching blocks, which propagates back-pressure to the peer. |
| `MaxConcurrentAttempts` | `int` | `2` | max `16` | Punch sessions one Listener or Dialer runs at the same time. Further sessions wait. |

Independently of these, each Listener or Dialer has a candidate socket budget
of 512. A direct attempt takes 2 sockets, a predictive attempt
`Predictive.Concurrency + 1`, and a birthday attempt `Birthday.Candidates`.
With the defaults, two concurrent birthday attempts (2 x 256) use the whole
budget.

### Strategy

| Field | Type | Default | Description |
|---|---|---|---|
| `Strategy` | `Strategy` | see below | `StrategyDirect` (`"direct"`), `StrategyPredictive` (`"predictive"`), `StrategyBirthday` (`"birthday"`) or `StrategyAuto` (`"auto"`). |
| `StableSide` | `bool` | `false` | Listener only: declares that this host's NAT keeps a stable, destination-independent mapping. Required by `Listen` for `birthday` and `auto`, and by `auto` before it picks predictive or birthday. A fixed `predictive` Listener does not check it, but predictive still assumes the Listener is the stable side. |
| `PortScanRange` | `int` | `0` | Legacy. Dialer only; `0..65535`. Used as `Predictive.PortScanRange` when that is zero. |
| `Predictive` | `PredictiveOptions` | | See below. |
| `Birthday` | `BirthdayOptions` | | See below. |
| `NATProbe` | `NATProbeOptions` | | See below. |

When `Strategy` is empty, the side uses `predictive` if `PortScanRange > 0` and
`direct` otherwise, and the Dialer lets the Listener pick. Prefer setting it
explicitly.

### `PredictiveOptions`

| Field | Default | Limits | Set on | Description |
|---|---|---|---|---|
| `PortScanRange` | `512` | `1..65535` | Dialer | Width of the port window the Listener scans, centered on the Dialer's reflected port. |
| `Concurrency` | `64` | max `511` | either | Parallel dials per scan round. |
| `AttemptTimeout` | `2s` | | either | Total duration of the Listener's scan. |

### `BirthdayOptions`

All fields are Dialer-only.

| Field | Default | Limits | Description |
|---|---|---|---|
| `Selection` | none | required for `birthday` and `auto` | `CandidateSelectionACK` (`"ack"`) or `CandidateSelectionFanout` (`"fanout"`). |
| `Candidates` | `256` | max `512` | Candidate connections each side opens. |
| `MinPort` | `1024` | | Lowest destination port (inclusive) the Listener picks from. |
| `MaxPort` | `65535` | `MinPort <= MaxPort`, and the range must hold at least `Candidates` ports | Highest destination port (inclusive). |
| `Warmup` | `250ms` | max `10s` | Time the Listener waits after arming its candidate sockets before signaling READY. |
| `AttemptTimeout` | `3s` | max `30s`; `Warmup + AttemptTimeout` must not exceed the Listener's `PunchTimeout` | Maximum time for the collision phase. |
| `SuccessGrace` | `250ms` | max `5s` | Fanout only: how long the Listener keeps collecting hits after the first one. |
| `MaxSuccessCandidates` | `8` | max `16` | Fanout only: maximum connections delivered to the Listener's `Accept` for one Dial. |

With `ack`, the two sides exchange a 73-byte HMAC-authenticated selection
frame on candidate connections so one Dial and one Accept agree on the same connection; the
frame is consumed inside the SDK. With `fanout`, the Dialer returns the first
hit and the Listener delivers up to `MaxSuccessCandidates` hits, leaving the
upper-layer protocol to discard the ones that do not lead anywhere.

The default 256 candidates over ports 1024-65535 give a single-round collision
probability of roughly 64%. More candidates raise it, at the cost of file
descriptors, NAT table entries and short bursts of SYNs.

### `NATProbeOptions`

Used only by a Dialer with `Strategy: auto`.

| Field | Default | Limits | Description |
|---|---|---|---|
| `Targets` | `[SignalAddr]` | `auto` needs at least 3 distinct `host:port` | Signal server addresses that serve `?command=nat` (same format as `SignalAddr`, prefix included). Targets on two different public IPs let the profile verify cross-IP behavior. |
| `InitialSamples` | `8` | min `3` | Samples in the first batch. Further batches take 8 samples. |
| `MaxSamples` | `24` | `InitialSamples..24` | Maximum samples while the class is still unknown. |
| `Timeout` | `5s` | | Per-target WebSocket timeout. The whole probe is bounded by `Timeout x MaxSamples` and by the Dial context. |
| `CacheTTL` | `10m` | | How long a profile is cached. An unknown result is cached for at most 30 s. |

Each sample binds one local port, opens a mapping lease to every target from
that port, and compares the public IP and port each target observed. The
cached profile is dropped when the interface or target list changes, when the
public IP no longer matches, or after two consecutive punch or selection
failures.

### Logging

| Field | Type | Description |
|---|---|---|
| `InfoLog` | `LogFunc` | Milestones: pairing complete, punch succeeded (with local and remote address and elapsed time). |
| `DebugLog` | `DebugLogFunc` | Diagnostics. When nil, diagnostics go to the standard `log` package only if `BURROW_DEBUG=1` is set or `burrow.SetDebug(true)` was called. When set, it also receives a `tcp_stats` line for every established connection once per second (Linux and macOS). |
| `StatsLog` | `func(TCPStats)` | Structured TCP statistics for every established connection, once per second (Linux and macOS). Independent of the log callbacks. |

`LogFunc` is `func(format string, args ...any)`; `DebugLogFunc` is an alias.

### Validation summary

`NewDialer` and `Listen` return an error when:

- `SignalAddr` or `Room` is empty, or `Room` is blank or longer than 128 bytes;
- `AddressFamily` is not `ipv4` or `ipv6`;
- `Strategy` is not one of the four values;
- `PortScanRange` or `Predictive.PortScanRange` is outside `0..65535`;
- `Predictive.Concurrency > 511`, `MaxConcurrentAttempts > 16`;
- `NATProbe.InitialSamples < 3`, `MaxSamples < InitialSamples` or `MaxSamples > 24`;
- (Dialer) `Birthday.Candidates > 512`, `Birthday.MaxSuccessCandidates > 16`,
  `Birthday.Warmup > 10s`, `Birthday.AttemptTimeout > 30s`, `Birthday.SuccessGrace > 5s`,
  the birthday port range is inverted or smaller than `Candidates`, or
  `Birthday.Selection` is not `ack`/`fanout` with `birthday` or `auto`;
- (Dialer) `auto` with fewer than three distinct NAT probe targets;
- (Listener) `Birthday`, `Predictive.PortScanRange` or `PortScanRange` is set;
- (Listener) `birthday` or `auto` without `StableSide`.

A Listener also refuses a DIAL whose parameters exceed its local limits
(`room_busy` for too many birthday candidates or success candidates) or whose
fingerprint does not match the parameters it carries (`strategy_mismatch`).

### Strategy matching

| Dialer | Listener | Result |
|---|---|---|
| fixed `X` | `auto` | `X` (Listener must have `StableSide` for birthday) |
| fixed `X` | fixed `X` | `X` |
| fixed `X` | fixed `Y` | `strategy_mismatch` |
| `auto` | `auto` | chosen from the Dialer's NAT profile: stable -> direct, sequential -> predictive, dispersed -> birthday, unknown -> `ErrUnsupportedNATPair` |
| `auto` | fixed `Y` | `Y` |

## `burrow.PacketConfig` (experimental)

`PacketConfig` configures the UDP packet API (`ListenPacket`,
`NewPacketDialer`). Fields shared with `Config` (`SignalAddr`, `Mode`, `Room`,
`ResolveIP`, `BindInterface`, `InterfaceName`, `InterfaceAddr`, `Control`,
`DebugLog`, `InfoLog`, `StableSide`) have the same meaning. Signaling always
uses IPv4.

| Field | Default | Limits | Description |
|---|---|---|---|
| `STUN.Servers` | required | at least one | STUN servers (`host:port`) used to learn each candidate socket's public mapping. |
| `STUN.InitialSamples` | `3` | | First sample batch for the UDP NAT profile. |
| `STUN.MaxSamples` | `8` | `>= InitialSamples` | Maximum profile samples. |
| `STUN.Timeout` | `5s` | | Total time for one STUN Binding query, including retransmissions. |
| `STUN.CacheTTL` | `10m` | | Profile cache lifetime. |
| `PairTimeout` | `60s` | | As in `Config`. |
| `PunchTimeout` | `20s` | | Maximum time for one UDP punch. |
| `Strategy` | required | `direct` or `predictive` | `birthday` and `auto` are rejected for now. |
| `Predictive.PortScanRange` | `512` | max `4096` | UDP port window. |
| `Predictive.Concurrency` | `64` | max `511` | |
| `Predictive.AttemptTimeout` | `3s` | | |
| `Birthday` | | | Accepted for signature stability; not used yet. |
| `MaxConcurrentAttempts` | `2` | max `16` | |
| `SignProof` / `VerifyProof` | nil | | Optional hooks that sign and verify the DIAL/READY signaling messages. The Listener verifies before allocating any candidate socket. `NewProofSigner(password)` and `NewProofVerifier(password)` provide an HMAC-SHA256 implementation keyed from a shared password. |

Candidate packets are rate-limited to 4096 packets per second with a burst of
1024.

## Tunnel JSON configuration

`burrow tunnel client -c client.json` listens on a local TCP address and
punches one hole per accepted connection; `burrow tunnel server -c
server.json` forwards every punched hole to a fixed local TCP address. Bytes
are spliced as-is in both directions, with no header, handshake or
multiplexing. Durations are strings such as `"250ms"` or `"3s"`.

### Common fields

| Field | Required | Default | Description |
|---|---|---|---|
| `signal` | yes | | Signal server address, same format as `Config.SignalAddr`. |
| `room` | yes | | Room name, identical on both sides. |
| `bind_interface` | yes | | Physical interface to punch from. The tunnel also binds its sockets to this device, so a TUN device that owns the default route cannot capture punching or signaling traffic. |
| `strategy` | yes | | `direct`, `predictive` or `birthday`. `auto` is not supported by the tunnel. |
| `mode` | no | `""` | `""` or `"header"`, as `Config.Mode`. |
| `hosts` | no | | Static host name to IP map for the signal server, bypassing DNS. Names not listed fall back to the system resolver (IPv4). |
| `log_level` | no | `info` | `debug`, `info`, `warn` or `error`. |
| `max_concurrent_attempts` | no | `2` | As `Config.MaxConcurrentAttempts` (max 16). |
| `predictive.concurrency` | no | `64` | Local, either side. |
| `predictive.attempt_timeout` | no | `2s` | Local, either side. |

### Client-only fields

| Field | Required | Default | Description |
|---|---|---|---|
| `listen` | yes | | Local TCP listen address, such as `127.0.0.1:1080`. Whatever runs on top (SOCKS5 or anything else) is up to you. |
| `trials` | no | `100` | Maximum consecutive punch rounds per accepted connection. Birthday relies on repeated rounds; raise it for difficult NAT pairs. Must not be negative. |
| `predictive.port_scan_range` | no | `512` | Negotiated window width. |
| `birthday` | for `birthday` | | Object with the `BirthdayOptions` fields in snake case: `selection` (required, `ack` or `fanout`), `candidates`, `min_port`, `max_port`, `warmup`, `attempt_timeout`, `success_grace`, `max_success_candidates`. |

### Server-only fields

| Field | Required | Default | Description |
|---|---|---|---|
| `forward` | yes | | Local TCP address every punched hole is forwarded to. |
| `stable_side` | for `birthday` | `false` | As `Config.StableSide`. Not checked for `predictive`, but predictive assumes the server is the stable side. |
| `skip_nat_check` | no | `false` | Skip the startup self-check, which refuses to run the server when a UDP STUN probe reports a symmetric NAT. The probe is only an approximation of TCP behavior; a failed probe never blocks startup. |

A server config containing `birthday` or `predictive.port_scan_range` is
rejected: those values come from the client.

### Example

Client:

```json
{
  "listen": "127.0.0.1:1080",
  "signal": "ws://signal.example.com:9960/signal",
  "room": "my-room",
  "bind_interface": "en0",
  "strategy": "birthday",
  "mode": "header",
  "hosts": {
    "signal.example.com": "203.0.113.10"
  },
  "max_concurrent_attempts": 2,
  "birthday": {
    "selection": "ack",
    "candidates": 256,
    "min_port": 1024,
    "max_port": 65535,
    "warmup": "250ms",
    "attempt_timeout": "3s",
    "success_grace": "250ms",
    "max_success_candidates": 8
  },
  "log_level": "info"
}
```

Server:

```json
{
  "forward": "127.0.0.1:29999",
  "signal": "ws://signal.example.com:9960/signal",
  "room": "my-room",
  "bind_interface": "eth0",
  "strategy": "birthday",
  "mode": "header",
  "stable_side": true,
  "hosts": {
    "signal.example.com": "203.0.113.10"
  },
  "max_concurrent_attempts": 2,
  "predictive": {
    "concurrency": 64,
    "attempt_timeout": "5s"
  },
  "log_level": "info"
}
```

The signal server for this example would run with
`burrow-signal -listen :9960 -prefix /signal`.

## Troubleshooting checklist

1. `room` and `signal` are identical on both sides, and the signal ports are
   open for TCP in every firewall and security group.
2. The Listener (tunnel server) is the side behind the stable NAT and sets
   `StableSide` / `stable_side`.
3. Strategy parameters are set only on the Dialer (tunnel client).
4. Punching sockets are bound to the physical interface, especially on hosts
   with a TUN device or virtual bridges.
5. `?command=nat` timeouts with `auto`: test each probe target's
   `?command=health` (expects HTTP 204) and `?command=nat` individually.
6. `strategy_mismatch`: compare the strategies on both sides (see the matching
   table) and make sure both run the same protocol version.
7. `room_busy` or file-descriptor pressure: lower `Birthday.Candidates`,
   `Predictive.Concurrency` or `MaxConcurrentAttempts`, and reduce how many
   connections the upper layer opens in parallel.
