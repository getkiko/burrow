# mobile: Android bindings for the tunnel

Package `mobile` wraps the [`tunnel`](../tunnel) package for gomobile so the
Android app (see [`../android`](../android)) can run a Burrow client or
server, plus the STUN and TCP NAT diagnostics from [`diag`](../diag).

## Building

From the `burrow/` directory:

```bash
make aar-install   # one-time: install gomobile/gobind (github.com/sagernet/gomobile)
make aar           # build build/tunnel.aar
make apk           # build the .aar, copy it into the Android project, build a debug APK
```

Override the API level, Java package or version:

```bash
make aar ANDROID_API=26 JAVA_PKG=com.example.tunnel MOBILE_VERSION=1.0.0
```

The defaults are `ANDROID_API=23`, `JAVA_PKG=com.getkiko.burrow` (so the
classes are `com.getkiko.burrow.mobile.Mobile` and `...mobile.Observer`) and
`MOBILE_VERSION=dev`. The version is injected with `-ldflags -X` into
`mobile.version` and returned by `Mobile.version()`.

Build constraints:

- **`-androidapi` must be set.** gomobile defaults to API 16, which current
  NDKs reject. It must match the app's `minSdk` (23).
- **The gomobile fork must stay in `go.mod`.** The bindings are built with
  `github.com/sagernet/gomobile`, not `golang.org/x/mobile`. No Go code
  imports it, so `tools.go` pins it; otherwise `go mod tidy` drops it and
  `gomobile bind` fails.
- **`-ldflags -checklinkname=0` is required.** `github.com/wlynxg/anet`
  references `net.zoneCache` via `go:linkname`, which the Go 1.23+ linker
  rejects by default (`invalid reference to net.zoneCache`).
- `gomobile init` is not needed, and gomobile falls back to
  `~/Library/Android/sdk` when `ANDROID_HOME` is unset.

## API

Config and status are passed as **JSON strings**: gomobile cannot bind maps,
nested structs, `time.Duration`, `uint64` or cross-package pointers, and JSON
lets fields evolve without changing the binding layer and the Kotlin side in
lockstep. The config JSON has the same format as the `burrow tunnel` client
and server config files.

```java
// Lifecycle
Mobile.startClient(String configJSON, Observer observer) throws Exception
Mobile.startServer(String configJSON, Observer observer) throws Exception
Mobile.stop() throws Exception
String Mobile.snapshot()

// Helpers
String Mobile.validateConfig(String role, String configJSON)  // "" means valid
String Mobile.version()

// Diagnostics
String Mobile.runSTUN(String server, String iface, long timeoutMillis) throws Exception
String Mobile.runTCPNAT(String server, String ports, String iface, String proxy,
                        long localPort, long rounds, long timeoutMillis) throws Exception
```

Go `int` parameters are bound as Java `long`. `validateConfig` takes the role
`"client"` or `"server"` and applies the same validation as a real start.

`Observer` is implemented in Kotlin:

```kotlin
class MyObserver : Observer {
    override fun onStateChange(statusJSON: String) { ... }
    override fun onStats(statsJSON: String) { ... }
    override fun onLog(level: String, line: String) { ... }  // debug/info/warn/error
}
```

## Things to know

**Callbacks are synchronous JNI calls.** The `Observer` methods are not
delivered asynchronously; heavy work in them blocks the Go goroutine. Post UI
updates to the main thread. The binding never holds its own lock while
calling back, so calling `snapshot()` or `stop()` from a callback does not
deadlock.

**Go panics are recovered.** Every exported function recovers from panics and
turns them into an exception (or a fallback value). gomobile does not do this
itself: an unrecovered panic kills the whole app process. Panics in your
`Observer` implementation are also contained.

**Single global instance.** Only one tunnel runs at a time. A second
`startClient`/`startServer` throws instead of replacing the running tunnel;
call `stop()` before switching role or config. `stop()` is safe to call at any
time, including while a start is still in progress (the start is then
abandoned).

## Status JSON

`snapshot()` and `onStateChange` return the same structure:

| Field | Type | Description |
| --- | --- | --- |
| `State` | string | `stopped` / `idle` / `punching` / `connected` / `punch_failed` |
| `Room` | string | Room name |
| `LocalAddr` / `RemoteAddr` | string | Punched connection addresses |
| `Peers` | string[] | Active peer addresses (always an array, never null) |
| `UptimeMillis` | long | Connection uptime in milliseconds |
| `SessionCount` | int | Open punched connections (one per upstream connection) |
| `ActiveStreams` | int | Connections currently being forwarded |
| `TotalStreams` | long | Connections forwarded so far |
| `Queued` | int | Connections waiting to be punched (including the current one) |
| `PunchTrial` / `PunchTrials` | int | Current trial and total trials while punching |
| `PunchReason` | string | Why the last punch round failed (empty for an ordinary miss) |
| `AppRxBytes` / `AppTxBytes` | long | Bytes relayed at the application level |
| `MaxDownMbps` / `MaxUpMbps` | double | Peak bandwidth since start (added by the binding) |
| `LastError` | string | Most recent error |

`onStats` delivers a TCP sample every 5 seconds:

| Field | Type | Description |
| --- | --- | --- |
| `Role` | string | `dialer` / `listener` |
| `LocalAddr` / `RemoteAddr` | string | Sampled connection |
| `IntervalMillis` | long | Sample window |
| `RxBytes` / `TxBytes` | long | Bytes in this window |
| `RxPackets` / `TxPackets` / `RetransPackets` | long | Packets in this window |
| `RetransBytes` | long | Retransmitted bytes in this window |
| `TotalRxBytes` / `TotalTxBytes` / `TotalRetransBytes` | long | Cumulative bytes |
| `RTTMillis` | long | Round-trip time |
| `CongestionWindow` / `ReceiveWindow` / `ReceiveSpace` | long | Congestion window, local receive window and (Linux only) receive-buffer estimate, in bytes |

Throughput for a window is `RxBytes * 8 / (IntervalMillis / 1000) / 1e6` Mbps.

## Diagnostics JSON

`runSTUN` returns `NATType`, `NATLabel` (a human-readable type name),
`PublicAddr`, `IsSymmetric` and `Detail`; `runTCPNAT` returns `Detail`. Both
throw if the diagnostic fails.
