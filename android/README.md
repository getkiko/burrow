# Burrow for Android

An Android client for the Burrow TCP tunnel, written in Kotlin with Jetpack
Compose. The tunnel itself is the Go `tunnel` package, compiled into an `.aar`
with gomobile (see [`../mobile`](../mobile)).

The app runs either end of a tunnel:

- **Client**: listens on a local TCP address (default `127.0.0.1:10809`).
  Point any app on the device (a proxy client, an SSH client, ...) at that
  address; each connection is carried over a hole-punched TCP connection to
  the server.
- **Server**: accepts punched connections from a client in the same room and
  forwards them to a target address (`forward`).

Both ends need the same signal server and room name. The app shows the
tunnel state, live throughput and TCP quality, keeps a log, and includes
STUN and TCP NAT diagnostics.

Package name: `com.getkiko.burrow`. Minimum Android version: 6.0 (API 23).

## Building

Requirements:

- Go (see `go.mod`), the Android SDK and NDK
- JDK 17 for Gradle (see below)

From the `burrow/` directory:

```bash
make aar-install   # one-time: install gomobile/gobind from github.com/sagernet/gomobile
make aar           # build build/tunnel.aar
make apk           # build the .aar, copy it to android/app/libs/, run assembleDebug
```

The debug APK ends up in `android/app/build/outputs/apk/debug/app-debug.apk`.
`make aar-copy` only builds and copies the `.aar`; after that you can work in
`android/` with Gradle (or Android Studio) directly:

```bash
cd android
./gradlew assembleDebug
./gradlew installDebug
```

`android/app/libs/tunnel.aar` is a build artifact and is not checked in.

### Build notes

- **API level.** The `.aar` is built with `-androidapi 23` (`ANDROID_API` in
  the Makefile), which must match `minSdk` in `app/build.gradle.kts`. gomobile
  defaults to 16, which current NDKs reject.
- **gomobile fork.** The bindings use `github.com/sagernet/gomobile`, not
  `golang.org/x/mobile`. It must stay in `go.mod` (`mobile/tools.go` pins it).
- **Binding package.** The Makefile passes `-javapkg com.getkiko.burrow`, so
  the generated classes are `com.getkiko.burrow.mobile.Mobile` and
  `com.getkiko.burrow.mobile.Observer`.
- **JDK 17.** Gradle 8.13 crashes under newer JDKs (for example JDK 25) with
  only a bare version string as the error. Set the JDK in
  `~/.gradle/gradle.properties`:

  ```properties
  org.gradle.java.home=/path/to/jdk-17
  ```

  or set `JAVA_HOME`. Putting it in `android/local.properties` does not work:
  that file is read by the Android Gradle Plugin, not by the Gradle wrapper.
- **No View-based Material library.** The app is pure Compose; the XML theme
  only needs a resolvable parent, so it uses the platform
  `android:Theme.Material.Light.NoActionBar`. Adding
  `com.google.android.material` would pull in AppCompat, RecyclerView and
  friends for no benefit.

## Install and run

```bash
adb install -r app/build/outputs/apk/debug/app-debug.apk
adb shell am start -n com.getkiko.burrow/.MainActivity
adb logcat | grep -iE "burrow|AndroidRuntime|FATAL"
```

On first launch the app asks for the notification permission (Android 13+)
and to be exempted from battery optimizations. The exemption matters on
vendor ROMs that freeze the network of background apps.

## Layout

```
MainActivity       single Activity, Compose entry point, binds the service
TunnelService      foreground service that owns the tunnel
TunnelState        status/stats data classes and JSON parsing
ConfigStore        config storage (SharedPreferences)
NetworkBinder      binds the process to a physical network, bypassing VPNs
NetworkInterfaces  network interface enumeration
ui/
  App              three-tab scaffold
  TunnelScreen     tunnel tab: role, status, connect address, live stats
  LogScreen        log tab
  DiagScreen       debug tab: STUN and TCP NAT checks
  ConfigScreen     settings screen
  SettingRow       shared setting rows and dialogs
```

## Design notes

- **The service owns the tunnel; the UI only observes.** The tunnel and the
  log buffer survive Activity recreation (rotation, backgrounding).
- **Observer callbacks are synchronous JNI calls.** The Go side blocks until
  they return, so the Kotlin implementations only push into a `StateFlow` and
  never do IO, wait, or call back into `Mobile`.
- **Status comes from two paths.** State transitions (punching, connected,
  failed) are pushed through `onStateChange`; frequently changing counters
  such as `ActiveStreams` are polled once a second with `Mobile.snapshot()`.
- **Bypassing other VPNs.** When another app's VpnService is active, the
  client binds the process to a physical network with
  `ConnectivityManager.bindProcessToNetwork` before starting, so signaling
  and punching traffic does not go through the VPN. If binding fails, the
  start is aborted.
- **Interface per start.** The outbound interface is chosen each time the
  tunnel starts, because interface names change with the network. If the
  interface disappears, the tunnel stops.

## Limitations

- The app is only the tunnel. It does not provide a VpnService/tun; point
  your own apps at the local listen address.
- Logs are kept in memory only and are cleared when the app restarts.
- Less common config fields (`mode`, `hosts`, ...) are edited in the full
  JSON editor in Settings.
