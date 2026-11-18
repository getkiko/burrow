package com.getkiko.burrow

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.graphics.drawable.Icon
import android.os.Binder
import android.os.Build
import android.os.IBinder
import com.getkiko.burrow.mobile.Mobile
import com.getkiko.burrow.mobile.Observer
import java.util.concurrent.atomic.AtomicLong
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull

/**
 * Foreground service that owns the tunnel.
 *
 * The UI is only an observer: the tunnel keeps running while the Activity is
 * destroyed and recreated, and the log buffer lives here so it survives
 * screen rotation.
 *
 * Since Android 8, background processes are reclaimed within minutes, so this
 * must be a foreground service with a persistent notification.
 */
class TunnelService : Service() {

    private val binder = LocalBinder()
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    private val _status = MutableStateFlow(TunnelStatus())
    val status: StateFlow<TunnelStatus> = _status.asStateFlow()

    private val _stats = MutableStateFlow<TunnelStats?>(null)
    val stats: StateFlow<TunnelStats?> = _stats.asStateFlow()

    private val _logs = MutableStateFlow<List<LogLine>>(emptyList())
    val logs: StateFlow<List<LogLine>> = _logs.asStateFlow()

    /**
     * Monotonic sequence number for status updates, so polling cannot overwrite
     * a newer state pushed by a callback.
     *
     * _status has two concurrent writers: refreshStatus(), polled every second,
     * and the onStateChange callback. Between issuing Mobile.snapshot() and
     * getting its result, a callback may push a newer state; writing the poll
     * result then would roll the UI back to a stale snapshot until the next poll.
     *
     * So writes are compare-and-set on a sequence number: the poller records the
     * sequence before calling snapshot() and only writes (and advances the
     * sequence) if no callback advanced it in the meantime. Callbacks write
     * unconditionally and advance the sequence, since they are authoritative
     * events from the binding layer. A stale poll result is therefore always
     * rejected.
     *
     * This only prevents poll-vs-callback overwrites, not callback-vs-callback
     * ones: applyStatus() writes callbacks unconditionally, with no ordering
     * between two concurrent onStateChange calls. On the server, several session
     * goroutines may fire callbacks concurrently, so they can land out of order.
     * No lock is added for this: the window is normally corrected by the next
     * poll or callback (at most a brief flicker), which does not justify the
     * complexity and blocking cost of a lock.
     */
    private val statusSeq = AtomicLong(0)

    /**
     * Whether we are in the window between tapping Start and
     * Mobile.startXxx() returning.
     *
     * During this window the Go side has no state yet (the server's NAT
     * self-check takes up to 30 seconds and runs before
     * setState(StatePunching)), so snapshot() keeps returning "stopped". The
     * per-second poll started in onCreate() would faithfully write that
     * "stopped" into _status and knock STATE_STARTING back to "Stopped" within a
     * second. So polling skips its write during this window.
     *
     * Only polling is blocked, not callbacks: once the Go side pushes a state
     * (onStateChange) it is authoritative and must replace the optimistic state
     * immediately, or punching/connected would never show.
     */
    private val starting = java.util.concurrent.atomic.AtomicBoolean(false)

    /**
     * Binds the process to a physical network, bypassing another app's
     * VpnService / a system-wide VPN.
     *
     * lazy rather than a field initializer: the Service is a Context, but field
     * initializers run before the base context is attached, so
     * NetworkBinder(this) there would get an unattached instance. lazy defers it
     * to first use (startTunnel), when the Context is ready.
     */
    private val networkBinder by lazy {
        // Diagnostic logs go straight to the log screen: a bind failure aborts the
        // start, and the user must be able to see which step failed.
        NetworkBinder(this) { level, text -> appendLog(level, text) }
    }

    /**
     * Wake lock that keeps the tunnel alive with the screen off.
     *
     * A foreground service only keeps the process from being killed, not the CPU
     * from sleeping. With the screen off the system enters Doze, the tunnel's
     * smux keepalive (every 10 s) gets deferred, and after KeepAliveTimeout
     * (30 s) the connection is declared dead: the tunnel drops whenever the app
     * is backgrounded or the screen turns off.
     *
     * PARTIAL_WAKE_LOCK keeps only the CPU awake, without turning on the screen;
     * it is the standard approach for long-running network services.
     */
    private var wakeLock: android.os.PowerManager.WakeLock? = null

    private fun acquireWakeLock() {
        if (wakeLock?.isHeld == true) return
        val power = getSystemService(Context.POWER_SERVICE) as? android.os.PowerManager ?: return
        wakeLock = power.newWakeLock(
            android.os.PowerManager.PARTIAL_WAKE_LOCK,
            "burrow:tunnel",
        ).apply {
            setReferenceCounted(false)
            try {
                acquire()
            } catch (e: Throwable) {
                // A few ROMs restrict wake locks; failing to get one only weakens
                // keep-alive and must not stop the tunnel from starting.
            }
        }
    }

    private fun releaseWakeLock() {
        try {
            wakeLock?.takeIf { it.isHeld }?.release()
        } catch (e: Throwable) {
            // Ignored: nothing can be done if release fails.
        }
        wakeLock = null
    }

    inner class LocalBinder : Binder() {
        val service: TunnelService get() = this@TunnelService
    }

    override fun onBind(intent: Intent?): IBinder = binder

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
        // Poll every second: fields like ActiveStreams change too often to push each change through a JNI callback.
        scope.launch {
            while (true) {
                refreshStatus()
                delay(1000)
            }
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopTunnel()
            stopSelf()
            return START_NOT_STICKY
        }
        startForeground(NOTIFICATION_ID, buildNotification(_status.value))
        return START_STICKY
    }

    override fun onDestroy() {
        // Stop synchronously. stopTunnel() only submits a coroutine to scope and
        // returns; the scope.cancel() right after would cancel that coroutine,
        // possibly before it runs, so Mobile.stop() would never be called and the
        // Go tunnel goroutines, smux sessions and sockets would all leak.
        //
        // But Mobile.stop() can block for up to 30 seconds in the worst case: the
        // tunnel package's Close() ends with an unbounded wg.Wait(), and the server
        // has a 30 s zombieCandidateTimeout. Android's ANR threshold for Service
        // lifecycle callbacks is usually a few seconds (shorter on some vendor
        // ROMs), so waiting indefinitely would trade a guaranteed leak for a
        // possible ANR. Hence the timeout: wait at most STOP_TIMEOUT_MILLIS, then
        // let the Service be destroyed. The Mobile.stop() call itself is not
        // interrupted (withTimeoutOrNull cancels the coroutine but cannot interrupt
        // a call already inside JNI) and finishes its cleanup in the background.
        // That is acceptable: the process is going away and the OS reclaims what
        // is left; what matters is that onDestroy never hangs into an ANR.
        runBlocking {
            withTimeoutOrNull(STOP_TIMEOUT_MILLIS) {
                withContext(Dispatchers.IO) {
                    try {
                        Mobile.stop()
                    } catch (e: Throwable) {
                        // There is nowhere to show an error on the destroy path; ignore.
                    }
                }
            }
        }
        // The Service may be destroyed without going through stopTunnel() (system
        // reclaim, task swiped away), which skips the unbind there, so unbind again
        // here. unbind() is idempotent.
        networkBinder.unbind()
        networkBinder.stopWatching()
        releaseWakeLock()
        scope.cancel()
        super.onDestroy()
    }

    /**
     * Starts the tunnel.
     *
     * Runs on a background thread: punching can take a dozen seconds (plus up
     * to 30 seconds of NAT self-check on the server), so the caller must never
     * block.
     *
     * Immediate feedback: the state switches to STATE_STARTING synchronously,
     * before scope.launch, so the UI reacts as soon as the user taps Start.
     * The server's NAT self-check runs inside startServer(), before the Go side
     * calls setState(StatePunching), and snapshot() returns "stopped" the whole
     * time; without the optimistic state the UI would show nothing for up to
     * 30 seconds.
     */
    fun startTunnel(role: String, configJSON: String, iface: String, onResult: (String?) -> Unit) {
        // The interface is not stored in the config; the user picks it at every
        // start because interface names change with the network and a stored one
        // goes stale. Inject the chosen interface into this start's config copy.
        val effectiveConfig = try {
            org.json.JSONObject(configJSON).put("bind_interface", iface).toString()
        } catch (e: Throwable) {
            configJSON
        }
        // Optimistic state: written synchronously, without waiting on any JNI
        // call. The room comes from the config so the status line can show it
        // right away.
        val room = try {
            org.json.JSONObject(effectiveConfig).optString("room")
        } catch (e: Throwable) {
            ""
        }
        starting.set(true)
        applyStatus(TunnelStatus(state = TunnelStatus.STATE_STARTING, room = room), expectSeq = null)

        // With the screen off, Doze defers the smux keepalive (every 10 s) until
        // the 30 s timeout declares the peer gone; this is the usual cause of the
        // tunnel dropping in the background.
        acquireWakeLock()

        // Bind the process to a physical network before the Go side creates any
        // socket.
        //
        // When another app's VpnService (a system-wide VPN) is active, the default
        // route goes through its tun interface. Signal connections and punching
        // packets on the default route would be captured by that VPN: at best they
        // take a detour and time out, at worst the signal server sees the VPN's
        // exit IP and the punching candidates are wrong from the start.
        //
        // Why not SO_BINDTODEVICE/SO_BINDTOIFINDEX on the Go side: the former needs
        // CAP_NET_RAW (app processes do not have it), the latter Linux 5.7+
        // (Android 11 ships 5.4). bindProcessToNetwork needs no permission, works
        // on API 23+, and is process-wide, so sockets created by Go benefit
        // automatically without passing a Network object into Go.
        scope.launch {
            // The server does not bind: it has no tun to bypass, and binding is
            // process-wide, so it would also pin the local forward connection to the
            // physical network and break the data path.
            if (role != "server") {
                appendLog("info", "Binding to physical network")
                val boundTo = withContext(Dispatchers.IO) { networkBinder.bind() }
                if (boundTo == null) {
                    // If binding fails, stop instead of falling back to the default route.
                    //
                    // Falling back looks like a gentle degradation but is the worst outcome:
                    // with a VPN/tun running, the default route is the tun, punching traffic
                    // is captured by the VPN and the signal server sees its exit IP. The
                    // candidates are wrong from the start, so the symptom is "signal works,
                    // punching never does", with only a long-scrolled-away warning to explain
                    // it. Better to stop right away and say why.
                    val reason = "Could not bind to a physical network; start aborted. See the log above."
                    appendLog("error", reason)
                    // CAS rather than an unconditional set: bind() can take a few seconds
                    // (including a 3 s requestNetwork wait when the getAllNetworks snapshot
                    // fails), and the user may have tapped Stop meanwhile, in which case
                    // stopTunnel() has already reset the UI to "Stopped". Writing now would
                    // replace that clean state with a stale error and make it look as if
                    // Stop did not work. A failed CAS means the start was stopped; skip
                    // the write (same as the normal path below).
                    if (!starting.compareAndSet(true, false)) {
                        onResult(null)
                        return@launch
                    }
                    releaseWakeLock()
                    // Leave the foreground, or the notification stays stuck at its
                    // pre-start content and cannot be dismissed: updateNotification in
                    // applyStatus returns early when !isRunning, so it would not even show
                    // the failure. Same handling as stopTunnel().
                    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
                        stopForeground(STOP_FOREGROUND_REMOVE)
                    } else {
                        @Suppress("DEPRECATION")
                        stopForeground(true)
                    }
                    applyStatus(
                        TunnelStatus(state = "stopped", room = room, lastError = reason),
                        expectSeq = null,
                    )
                    onResult(reason)
                    return@launch
                }
                appendLog("info", "Bound to $boundTo, bypassing VPN/tun routing")
            }
            val error = try {
                if (role == "server") {
                    Mobile.startServer(effectiveConfig, observer)
                } else {
                    Mobile.startClient(effectiveConfig, observer)
                }
                null
            } catch (e: Throwable) {
                e.message ?: "Start failed"
            }
            // The start call has returned (success or failure), so the optimistic
            // state is done; from now on the real Go state wins. Clear it before
            // refreshStatus(), or that refresh would be blocked by the optimistic
            // guard.
            //
            // compareAndSet rather than set(false): the user may have tapped Stop
            // during the self-check, in which case stopTunnel() already cleared
            // starting and switched the UI to "Stopped". The error from this start
            // (most likely caused by the interruption) is stale, and writing it would
            // replace the user's stop with a confusing failure. A failed CAS means
            // the start was stopped; skip the write.
            if (!starting.compareAndSet(true, false)) {
                onResult(null)
                return@launch
            }
            if (error != null) {
                // Start failed (including a NAT self-check refusal). The Go
                // snapshot() is now "stopped" with no error, so refreshStatus() would
                // lose the reason and the user would see "Stopped" with no explanation.
                // Write the error into the status explicitly so the status line shows
                // the Go error in full.
                applyStatus(
                    TunnelStatus(state = "stopped", room = room, lastError = error),
                    expectSeq = null,
                )
            } else {
                refreshStatus()
                // Stop if the interface disappears: the Go punching sockets are bound
                // to it, and without it they silently drop everything while the status
                // still says "Connected". Better to stop and say why than show a fake
                // "Connected".
                networkBinder.watchInterface(iface) {
                    appendLog("error", "Interface $iface went down; tunnel stopped")
                    stopTunnel()
                }
            }
            onResult(error)
        }
    }

    fun stopTunnel() {
        // Clear the optimistic flag first, then switch to "Stopped".
        //
        // Tapping Stop during the NAT self-check (up to 30 s) is perfectly
        // reasonable, but starting is still true then; if not cleared first,
        // refreshStatus() below would return early because of it and the UI would
        // stay stuck on "Starting" with Stop doing nothing. Switching state
        // synchronously gives immediate feedback without waiting for Mobile.stop().
        starting.set(false)
        applyStatus(TunnelStatus(state = "stopped"), expectSeq = null)
        releaseWakeLock()
        networkBinder.stopWatching()

        // Unbinding is required: binding is process-wide, so without it every later
        // network request from the app stays pinned to this network and does not
        // follow a switch to Wi-Fi.
        networkBinder.unbind()

        // Leave the foreground, which removes the notification. Without this the
        // foreground notification would keep saying the tunnel is running after
        // Stop, and it cannot be swiped away.
        //
        // STOP_FOREGROUND_REMOVE removes the notification explicitly; below API 24
        // the boolean overload does the same.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
            stopForeground(STOP_FOREGROUND_REMOVE)
        } else {
            @Suppress("DEPRECATION")
            stopForeground(true)
        }

        scope.launch {
            try {
                Mobile.stop()
            } catch (e: Throwable) {
                appendLog("error", "Stop failed: ${e.message}")
            }
            refreshStatus()
        }
    }

    @Synchronized
    fun clearLogs() {
        logBuffer.clear()
        _logs.value = emptyList()
    }

    /**
     * Polls and refreshes the status (every second).
     *
     * A poll result may be stale (a callback may have pushed a newer state
     * between issuing snapshot() and getting the result), so the sequence number
     * is recorded first and applyStatus() rejects stale results.
     */
    private fun refreshStatus() {
        // During the start window polling yields to the optimistic state: Go
        // would only answer "stopped" and knock "Starting" back to "Stopped".
        if (starting.get()) return
        val seqBefore = statusSeq.get()
        val status = try {
            parseStatus(Mobile.snapshot())
        } catch (e: Throwable) {
            TunnelStatus(state = "stopped", lastError = "Failed to read status: ${e.message}")
        }
        applyStatus(status, seqBefore)
    }

    /**
     * Applies one status update; shared by polling and callbacks so the
     * "assign _status + refresh notification" logic lives in one place.
     *
     * @param expectSeq non-null for poll results: written only if the sequence
     *   still equals [expectSeq] (no more authoritative update arrived after the
     *   caller issued snapshot()). null for callback pushes, which are written
     *   unconditionally and advance the sequence; callbacks are authoritative
     *   binding-layer events and take precedence over polling.
     */
    private fun applyStatus(status: TunnelStatus, expectSeq: Long?) {
        if (expectSeq != null) {
            if (!statusSeq.compareAndSet(expectSeq, expectSeq + 1)) {
                // A callback advanced the sequence after the poll was issued, so this
                // snapshot is stale; drop it rather than overwrite the newer _status.
                return
            }
        } else {
            statusSeq.incrementAndGet()
        }
        _status.value = status
        updateNotification(status)
    }

    /**
     * Event callbacks from the binding layer.
     *
     * These three methods are synchronous, blocking JNI calls: the Go goroutine
     * waits for them to return. So they only do instantaneous work such as
     * pushing into a flow, never IO or waiting, and must not call back into
     * Mobile (e.g. snapshot()): the ordering between Go's callback sites and its
     * state lock is not under our control, and a synchronous re-entry into JNI
     * risks deadlock. Frequently changing fields come from the per-second poll.
     */
    private val observer = object : Observer {
        override fun onStateChange(statusJSON: String) {
            applyStatus(parseStatus(statusJSON), expectSeq = null)
        }

        override fun onStats(statsJSON: String) {
            parseStats(statsJSON)?.let { _stats.value = it }
        }

        override fun onLog(level: String, line: String) {
            appendLog(level, line)
        }
    }

    /**
     * Appends a log line, dropping the oldest once full.
     *
     * ArrayDeque rather than List with "current + item"/"takeLast": those copy
     * the whole list twice per line (concat, then trim), which is O(n). This runs
     * inside the Observer callback (on the Go call stack), and logging is heavy
     * while punching, so O(n) copies would hold up the Go thread. ArrayDeque
     * append and drop-oldest are amortized O(1). The StateFlow still needs a new
     * List snapshot for subscribers, but it is copied once per change.
     */
    private val logBuffer = ArrayDeque<LogLine>(MAX_LOG_LINES)

    // Once logBuffer holds 1000 lines it drops the oldest as it appends, so its
    // size stays constant. If the UI (TunnelScreen/LogScreen) keyed its
    // auto-scroll LaunchedEffect on logs.size, auto-scroll would stop working
    // after 1000 lines. So each line gets a monotonically increasing number
    // (nextLogSeq) and the UI keys on the last line's seq instead.
    private var nextLogSeq = 0L

    // ArrayDeque is not thread-safe: appendLog runs on the Observer callback
    // thread (Go/JNI) while clearLogs() may be called from the UI thread via
    // the binder, so both are @Synchronized. The critical section is only an
    // O(1) add/remove plus one toList() snapshot, so JNI callers do not wait
    // noticeably.
    @Synchronized
    private fun appendLog(level: String, text: String) {
        if (logBuffer.size >= MAX_LOG_LINES) {
            logBuffer.removeFirst()
        }
        logBuffer.addLast(LogLine(level, text, seq = nextLogSeq++))
        _logs.value = logBuffer.toList()
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "Tunnel status",
                NotificationManager.IMPORTANCE_LOW,
            ).apply {
                description = "Shows tunnel connection status and live throughput"
                setShowBadge(false)
            }
            getSystemService(NotificationManager::class.java)
                .createNotificationChannel(channel)
        }
    }

    private fun buildNotification(status: TunnelStatus): Notification {
        val openIntent = PendingIntent.getActivity(
            this, 0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE,
        )
        val stopIntent = PendingIntent.getService(
            this, 1,
            Intent(this, TunnelService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE,
        )

        val stats = _stats.value
        val subtitle = when {
            status.isConnected && stats != null ->
                "↓ %.1f Mbps  ↑ %.1f Mbps".format(stats.downMbps, stats.upMbps)
            status.isConnected -> status.remoteAddr
            status.lastError.isNotEmpty() -> status.lastError.take(60)
            else -> status.room
        }

        val title = if (status.isConnected) {
            "Connected ${formatUptime(status.uptimeMillis)}"
        } else {
            status.displayName
        }

        val builder = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            Notification.Builder(this, CHANNEL_ID)
        } else {
            @Suppress("DEPRECATION")
            Notification.Builder(this)
        }

        // The Icon argument of Notification.Action.Builder rejects null on most
        // API levels, so use the built-in cancel icon.
        val stopIcon = Icon.createWithResource(this, android.R.drawable.ic_menu_close_clear_cancel)

        return builder
            .setContentTitle(title)
            .setContentText(subtitle)
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setContentIntent(openIntent)
            .addAction(Notification.Action.Builder(stopIcon, "Stop", stopIntent).build())
            .setOngoing(true)
            .build()
    }

    private fun updateNotification(status: TunnelStatus) {
        if (!status.isRunning) return
        try {
            getSystemService(NotificationManager::class.java)
                .notify(NOTIFICATION_ID, buildNotification(status))
        } catch (e: Throwable) {
            // A failed notification update must not affect the tunnel.
        }
    }

    companion object {
        const val ACTION_STOP = "com.getkiko.burrow.STOP"
        private const val CHANNEL_ID = "tunnel"
        private const val NOTIFICATION_ID = 1

        // Upper bound for waiting on Mobile.stop() in onDestroy(). A normal shutdown
        // (not stuck in the 30 s wg.Wait()) is far faster, so only a real hang hits
        // it; 3 s is well below common ANR thresholds while leaving ample headroom
        // over a typical JNI round trip.
        private const val STOP_TIMEOUT_MILLIS = 3000L
        private const val MAX_LOG_LINES = 1000

        fun start(context: Context) {
            val intent = Intent(context, TunnelService::class.java)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }
    }
}
