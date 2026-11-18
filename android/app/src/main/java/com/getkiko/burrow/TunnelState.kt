package com.getkiko.burrow

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

/** Tunnel status snapshot; mirrors the binding layer's status JSON. */
@Serializable
data class TunnelStatus(
    @SerialName("State") val state: String = "stopped",
    @SerialName("Room") val room: String = "",
    @SerialName("LocalAddr") val localAddr: String = "",
    @SerialName("RemoteAddr") val remoteAddr: String = "",
    @SerialName("UptimeMillis") val uptimeMillis: Long = 0,
    // Number of punched holes currently open. Each upstream connection gets its
    // own hole (one-to-one, no multiplexing), so this is the number of
    // connections running through the tunnel. On the client it is no longer
    // always 1 (it was when a single punched TCP connection was shared); it now
    // varies with concurrency.
    //
    // The JSON name stays SessionCount (same as on the Go side): renaming it
    // would need both sides to ship together, which is not worth it for wording.
    @SerialName("SessionCount") val sessionCount: Int = 0,
    // Connections currently being forwarded. Counted differently from
    // sessionCount: it also includes connections that have not triggered a
    // punch yet (lazy punching) and ones whose forward target is unreachable,
    // so it is always >= sessionCount.
    @SerialName("ActiveStreams") val activeStreams: Int = 0,
    // Total connections forwarded so far; only grows.
    @SerialName("TotalStreams") val totalStreams: Long = 0,
    @SerialName("MaxDownMbps") val maxDownMbps: Double = 0.0,
    @SerialName("MaxUpMbps") val maxUpMbps: Double = 0.0,
    @SerialName("Peers") val peers: List<String> = emptyList(),
    @SerialName("AppRxBytes") val appRxBytes: Long = 0,
    @SerialName("AppTxBytes") val appTxBytes: Long = 0,
    @SerialName("PunchTrial") val punchTrial: Int = 0,
    @SerialName("PunchTrials") val punchTrials: Int = 0,
    @SerialName("PunchReason") val punchReason: String = "",
    // Connections queued for punching (including the one being punched).
    // Punching is serial, so concurrent connections go one at a time; queued
    // ones do not advance punchTrial, so without showing this the UI would look
    // stuck with no explanation.
    @SerialName("Queued") val queued: Int = 0,
    @SerialName("LastError") val lastError: String = "",
) {
    val isRunning: Boolean get() = state != "stopped"
    val isConnected: Boolean get() = state == "connected"

    /**
     * Whether work is in progress; the UI uses it to decide whether to show a
     * progress indicator.
     *
     * Only STATE_STARTING (the optimistic Kotlin-side state while the NAT
     * self-check runs) and punching count. idle does not: it means the tunnel is
     * ready and waiting for the first read/write to trigger punching, and nothing
     * is busy in the background. A spinner there would make users wait for the
     * app when it is actually waiting for them to point their upstream at it.
     */
    val isBusy: Boolean get() = state == STATE_STARTING || state == "punching"

    /** The tunnel is ready but has not started punching (lazy connect, triggered by the first read/write). */
    val isIdle: Boolean get() = state == "idle"

    /** Whether this is a failure state; the UI uses error colors and offers a retry. */
    val isFailed: Boolean get() = state == "punch_failed" ||
        (!isRunning && lastError.isNotEmpty())

    /** Human-readable state label for the UI. */
    val displayName: String
        get() = when (state) {
            "stopped" -> "Stopped"
            // Role-independent label. Only the server runs the NAT self-check (the
            // client does not), so this must not say "detecting network type": that
            // would be false for the client. detailHint(role) explains what is
            // actually happening per role.
            STATE_STARTING -> "Starting"
            // idle is not "connecting": the Go side defines it as "not punched yet
            // (lazy connect, triggered by the first read/write)". The tunnel is ready
            // and listening, but punching only starts once the upstream sends data.
            // Calling it "connecting" would suggest the app is busy when it is
            // actually waiting for the user.
            "idle" -> "Ready"
            "punching" -> "Punching"
            "connected" -> "Connected"
            "punch_failed" -> "Punch failed"
            else -> state
        }

    /**
     * The small hint line under the state, telling the user what is being
     * waited on.
     *
     * The idle hint depends on the role, because the two ends wait for
     * different things. The client waits for the upstream app to open the first
     * connection to the local listen port (punching is lazy and does not start
     * without traffic); the server waits for the client to punch through.
     */
    fun detailHint(role: String): String {
        val base = baseHint(role)
        // The queue hint must show in every running state, not just punching.
        //
        // Queuing happens on the Go side before punch() is entered (punchMu), while
        // State still holds its previous value: connected if a hole is already up,
        // idle on the first connect, never punching. Attaching the queue hint only
        // to punching would hide it in the most common case (connected plus a new
        // connection queued), where the new connection just appears stuck.
        if (queued <= 1 || !isRunning) return base
        val queueHint = "${queued - 1} more queued"
        return if (base.isEmpty()) queueHint else "$base • $queueHint"
    }

    private fun baseHint(role: String): String = when (state) {
        // Only the server runs the NAT self-check (birthday strategy, up to 30
        // seconds); on the client this step just opens the listener and connects
        // to the signal server, which is quick.
        STATE_STARTING -> if (role == "server") {
            "Detecting NAT type, up to 30 s"
        } else {
            "Opening local listener and connecting to signal server"
        }
        "idle" -> if (role == "server") {
            "Ready, waiting for the client"
        } else {
            "Ready. Point your app at the local listen address to start punching"
        }
        "punching" -> buildString {
            // Progress plus reason: punching can run all 100 trials, so "trying"
            // alone says nothing. Showing "2/100 • <reason>" tells the user at a
            // glance whether this is normal collision probing or the peer is not
            // online at all, which call for very different actions.
            if (punchTrials > 0) {
                append("Trial $punchTrial/$punchTrials")
            } else {
                append("Trying a direct connection with the peer")
            }
            if (punchReason.isNotEmpty()) {
                append(" • ")
                append(punchReason)
            }
            // The queue hint is not added here: detailHint() appends it for every
            // running state, so adding it here would show it twice.
        }
        else -> ""
    }

    companion object {
        /**
         * Optimistic Kotlin-side state: the user tapped Start and
         * Mobile.startServer() has not returned yet.
         *
         * The Go side has no state to report during this time: the server's NAT
         * self-check runs inside startServer(), before setState(StatePunching), so
         * Mobile.snapshot() still returns "stopped". The self-check can take up to
         * 30 seconds (natCheckTimeout = diag.DefaultSTUNTimeout); without a state
         * for it the UI would show no reaction for 30 seconds after Start.
         *
         * This value exists only on the Kotlin side and never appears in any
         * status JSON from Go.
         */
        const val STATE_STARTING = "starting"
    }
}

/** TCP sample; mirrors the binding layer's stats JSON. */
@Serializable
data class TunnelStats(
    @SerialName("Role") val role: String = "",
    @SerialName("IntervalMillis") val intervalMillis: Long = 0,
    @SerialName("LocalAddr") val localAddr: String = "",
    @SerialName("RemoteAddr") val remoteAddr: String = "",
    @SerialName("RxBytes") val rxBytes: Long = 0,
    @SerialName("TxBytes") val txBytes: Long = 0,
    @SerialName("TotalRxBytes") val totalRxBytes: Long = 0,
    @SerialName("TotalTxBytes") val totalTxBytes: Long = 0,
    @SerialName("RetransBytes") val retransBytes: Long = 0,
    @SerialName("TxPackets") val txPackets: Long = 0,
    @SerialName("RetransPackets") val retransPackets: Long = 0,
    @SerialName("RxPackets") val rxPackets: Long = 0,
    @SerialName("TotalRetransBytes") val totalRetransBytes: Long = 0,
    @SerialName("ReceiveWindow") val receiveWindow: Long = 0,
    @SerialName("ReceiveSpace") val receiveSpace: Long = 0,
    @SerialName("RTTMillis") val rttMillis: Long = 0,
    @SerialName("CongestionWindow") val congestionWindow: Long = 0,
) {
    /** Download rate in this sample window (Mbps). */
    val downMbps: Double
        get() = if (intervalMillis <= 0) 0.0 else rxBytes * 8.0 / (intervalMillis / 1000.0) / 1e6

    /**
     * Retransmission rate (%): retransmitted packets as a share of sent packets
     * in this sample window.
     *
     * This is the most direct link-quality indicator. Punched direct paths vary
     * widely in quality, and a high retransmission rate says more about a bad
     * hole than RTT alone.
     */
    val retransRate: Double
        get() = if (txPackets <= 0) 0.0 else retransPackets * 100.0 / txPackets

    /** Cumulative retransmission rate (%), reflecting quality over the whole connection lifetime. */
    val totalRetransRate: Double
        get() = if (totalTxBytes <= 0) 0.0 else totalRetransBytes * 100.0 / totalTxBytes

    /** Upload rate in this sample window (Mbps). */
    val upMbps: Double
        get() = if (intervalMillis <= 0) 0.0 else txBytes * 8.0 / (intervalMillis / 1000.0) / 1e6
}

/**
 * One log line.
 *
 * [seq] is a monotonically increasing number assigned by the service, not an
 * array index. The log buffer holds 1000 lines and, once full, drops the
 * oldest as it appends, so its size stays constant: an index would point to
 * different lines over time (breaking LazyColumn keys) and gives no stable
 * signal that the log changed. seq only grows, so it works both as the
 * LazyColumn key and as the "new log arrived" signal.
 */
data class LogLine(
    val level: String,
    val text: String,
    val seq: Long,
    val timeMillis: Long = System.currentTimeMillis(),
)

/** Lenient parser: new fields in the binding layer must not crash the UI. */
private val tunnelJson = Json {
    ignoreUnknownKeys = true
    // Fall back to the field default on null instead of throwing. Go marshals
    // nil slices/pointers as null, and any such field would fail the whole
    // status parse and break the UI ("Expected start of the array"). The
    // binding layer already guarantees Peers is an array; this guards against
    // future fields.
    coerceInputValues = true
    isLenient = true
}

/** Parses status JSON; on failure returns a fallback carrying the error. */
fun parseStatus(json: String): TunnelStatus =
    try {
        tunnelJson.decodeFromString<TunnelStatus>(json)
    } catch (e: Throwable) {
        TunnelStatus(state = "stopped", lastError = "Failed to parse status: ${e.message}")
    }

/** Parses stats JSON; returns null on failure. */
fun parseStats(json: String): TunnelStats? =
    try {
        tunnelJson.decodeFromString<TunnelStats>(json)
    } catch (e: Throwable) {
        null
    }

/** Formats milliseconds as mm:ss or h:mm:ss. */
fun formatUptime(millis: Long): String {
    val totalSeconds = millis / 1000
    val hours = totalSeconds / 3600
    val minutes = (totalSeconds % 3600) / 60
    val seconds = totalSeconds % 60
    return if (hours > 0) {
        "%d:%02d:%02d".format(hours, minutes, seconds)
    } else {
        "%02d:%02d".format(minutes, seconds)
    }
}

/** Formats a byte count in human-readable form. */
fun formatBytes(bytes: Long): String = when {
    bytes >= 1L shl 30 -> "%.2f GB".format(bytes.toDouble() / (1L shl 30))
    bytes >= 1L shl 20 -> "%.1f MB".format(bytes.toDouble() / (1L shl 20))
    bytes >= 1L shl 10 -> "%.1f KB".format(bytes.toDouble() / (1L shl 10))
    else -> "$bytes B"
}
