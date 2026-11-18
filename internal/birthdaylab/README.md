# TCP birthday-collision experiment

Package `birthdaylab` implements an experiment that is exposed through the
CLI as `burrow birthday server|client`. It is a research tool, not part of the
public SDK.

## The question

One peer sits behind a NAT that keeps the same public mapping for every
destination (a "stable" side: an endpoint-independent or address-dependent
mapping, a public host, or a port forward). The other peer sits behind a NAT
that allocates a new, unpredictable public port for every connection
(address-and-port-dependent mapping, typical for carrier-grade NAT on mobile
networks). Can the two still open a direct TCP connection by brute force?

The experiment answers this with a birthday collision:

1. The server picks `candidates` random ports in `[min-port, max-port]` and,
   from one fixed local TCP port, sends a SYN to the client's public IP on each
   of them. The sockets stay in `SYN_SENT`.
2. After a short `warmup`, the server publishes its stable public endpoint
   through the signal server (`READY`).
3. The client opens `candidates` connections from different local ports to that
   endpoint. Each one gets its own public mapping from the client's NAT.
4. If any client mapping lands on one of the server's random ports, the two
   sides hold exactly reversed TCP 4-tuples and complete a TCP simultaneous
   open. Both sides then verify the connection with a session token.

With 256 x 256 candidates over ports 1024-65535 the theoretical single-trial
success rate is about 63.8%. The CLI prints the empirical rate next to the
theoretical one, plus the rate for three independent attempts.

## Build

```bash
make signal cli
```

This produces `build/burrow` and `build/burrow-signal-<os>-<arch>`.

## Run

Start a signal server that listens on three TCP ports. The extra ports let the
stable side verify that its mapping does not depend on the destination port:

```bash
./build/burrow-signal-linux-amd64 \
  -listen :9960,:9962,:9963 \
  -prefix /signal \
  -stat 127.0.0.1:9961
```

Signal addresses are written as `IP:port/path`, where the path matches the
signal server's `-prefix`. `-signal_path` (alias `-signal-path`) is still
accepted for older scripts.

### Stable side behind a NAT (`-mode nat`)

```bash
./build/burrow birthday server \
  -mode nat \
  -signal 203.0.113.10:9960/signal \
  -probe-signals '203.0.113.10:9960/signal,203.0.113.10:9962/signal,203.0.113.10:9963/signal' \
  -room birthday-demo \
  -iface en0
```

The server connects to every probe address from the same local port through
the `path?command=nat` entry point and prints the public mapping each one
observes. The experiment only starts when all observations agree.
`-probe-signals` accepts addresses separated by commas, semicolons, spaces or
newlines; without it the server probes the ports in `-probe-ports` on the
`-signal` host.

Probing several ports of one IP only shows the NAT is an EIM/ADM candidate.
Adding a probe address on a second public IP distinguishes EIM from ADM.

### Stable side with a fixed public endpoint (`-mode fixed`)

On a cloud host with a one-to-one public IP, or behind a port forward, give the
local port and the public endpoint explicitly. This skips the mapping probe and
isolates the client side of the measurement:

```bash
./build/burrow birthday server \
  -mode fixed \
  -signal 203.0.113.10:9960/signal \
  -room birthday-demo \
  -iface eth0 \
  -local-port 20000 \
  -public-address 198.51.100.20:20000
```

The server never listens on port 20000; it only dials out from it. The
firewall or security group must still allow inbound TCP to that port so the
client's simultaneous-open SYNs reach the host's TCP stack.

### Client

Run the client on the network under test, with the same `-signal` and `-room`:

```bash
./build/burrow birthday client \
  -signal 203.0.113.10:9960/signal \
  -room birthday-demo \
  -iface en0 \
  -candidates 256 \
  -trials 100 \
  -min-port 1024 \
  -max-port 65535 \
  -warmup 250ms \
  -attempt-timeout 3s
```

The client owns the trial parameters: candidates, port range, warmup and
attempt timeout travel to the server in each signal request and the server
adopts them per session. The client's `-trials` sets the number of trials; the
server keeps accepting sessions until interrupted. Use the interface that
carries the network under test (`eth0`, `wlan0`, `rmnet_data0` and so on).

Both commands are also available in the web console started by `burrow api`.

## Output

For every trial the CLI prints:

- the session ID and the number of armed server SYNs or client mappings;
- the matched local and public endpoints and the time to connect;
- how many candidates were armed, connected and validated;
- dial errors grouped as timeout, canceled, refused, resource and other;
- the cumulative success rate and average connect time.

A final line summarizes successes, the empirical and theoretical rates, and the
estimated success rate over three attempts.

See [NOTES.md](NOTES.md) for observations from earlier runs.
