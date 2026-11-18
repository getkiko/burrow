package com.getkiko.burrow.ui

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import androidx.compose.animation.AnimatedContent
import androidx.compose.animation.core.FastOutSlowInEasing
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.scaleIn
import androidx.compose.animation.togetherWith
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.FilledTonalIconButton
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.scale
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.getkiko.burrow.ConfigStore
import com.getkiko.burrow.TunnelService
import com.getkiko.burrow.listUsableInterfaces
import com.getkiko.burrow.TunnelStats
import com.getkiko.burrow.TunnelStatus
import com.getkiko.burrow.formatBytes
import com.getkiko.burrow.formatUptime
import kotlinx.coroutines.delay
import androidx.compose.ui.text.style.TextAlign
import org.json.JSONObject

/**
 * Tunnel home screen, the face of the app.
 *
 * Design rules:
 * 1. **No log lines on the home screen.** Logs are for developers and have
 *    their own tab. The home screen only says what the current state is,
 *    with one line of status text plus a visual.
 * 2. **Every state has its own visual**, not just a line of black text:
 *    in progress gets a pulse and a progress ring, success the primary color
 *    and a pop-in, failure the error color.
 * 3. **The connect address is the one thing the user has to act on**, so it
 *    is large, monospaced, visible at a glance and copyable with one tap.
 */
@Composable
fun TunnelScreen(
    service: TunnelService?,
    configStore: ConfigStore,
    onOpenConfig: () -> Unit,
    modifier: Modifier = Modifier,
) {
    if (service == null) {
        Column(modifier = modifier.padding(16.dp)) { Text("Connecting to service...") }
        return
    }

    val status by service.status.collectAsState()
    val stats by service.stats.collectAsState()
    var errorMessage by remember { mutableStateOf<String?>(null) }
    // The interface is not stored in the config but picked at every start:
    // interface names change with the network (mobile data <-> Wi-Fi, airplane
    // mode, hotspot reconnect), so a stored one goes stale, and a stale
    // interface does not error, it just never connects, which is very hard to
    // diagnose.
    var showIfacePicker by remember { mutableStateOf(false) }
    var showRoomEditor by remember { mutableStateOf(false) }

    // The role lives on the home screen, so it must be writable state:
    // configStore.role is a SharedPreferences property, not Compose State, and
    // changing it does not recompose. Keep a local mirror that drives the UI and
    // write it back to configStore on every change.
    var role by remember { mutableStateOf(configStore.role) }

    // The room name is read from the config: status.room is empty while
    // stopped, but the user still needs to see and edit it. Re-read keyed on
    // role + isRunning, same as configIssue (SharedPreferences is not Compose
    // State and does not refresh after returning from the config screen).
    var room by remember(role, status.isRunning) {
        mutableStateOf(
            runCatching { JSONObject(configStore.currentConfig).optString("room") }
                .getOrDefault(""),
        )
    }

    // Whether the config is ready to start, re-evaluated keyed on role +
    // isRunning: configStore.currentConfig reads SharedPreferences, not Compose
    // State, so it is not recomputed on return from the config screen.
    // isRunning always changes on start/stop, which forces a re-read so the
    // button state reflects the latest config.
    val configIssue = remember(role, status.isRunning) {
        configReadiness(configStore.currentConfig, role)
    }

    // Outer non-scrolling Column: the content area takes the remaining space
    // and scrolls on its own, while the action buttons stay pinned to the
    // bottom, where the most used Start button is easiest to reach.
    Column(
        modifier = modifier
            .fillMaxSize()
            .padding(horizontal = 20.dp, vertical = 16.dp),
    ) {
      Column(
        modifier = Modifier
            .weight(1f)
            .verticalScroll(rememberScrollState()),
    ) {
        // Role switcher. Disabled while the tunnel runs: switching roles then
        // would make the displayed config disagree with the running tunnel.
        RoleSwitcher(
            role = role,
            enabled = !status.isRunning,
            onRoleChange = {
                role = it
                configStore.role = it
                errorMessage = null
            },
        )

        Spacer(Modifier.height(28.dp))

        // Status area: the whole block transitions between states instead of swapping text.
        StatusHero(status, role, room) { showRoomEditor = true }

        // Big connect-address block, shown only for the client role while the
        // tunnel runs:
        //   - client: `listen` is the local TCP listen address; accepted
        //     connections are forwarded through the tunnel. This is the entry
        //     address the user points their upstream app at.
        //   - server: `forward` is where the server sends traffic out, the
        //     outbound direction, not an entry for the user.
        // So an IP:PORT for the user to enter only exists for the client. The
        // server shows what it is waiting for (room / forward target) instead,
        // so the user is not misled into entering forward.
        if (status.isRunning) {
            Spacer(Modifier.height(24.dp))
            if (role == "client") {
                val listen = remember(configStore.clientConfig) {
                    runCatching {
                        JSONObject(configStore.clientConfig).optString("listen")
                    }.getOrDefault("")
                }
                if (listen.isNotBlank()) {
                    EntryAddressCard(listen)
                }
            } else {
                ServerWaitingCard(status)
            }
        }

        // Connected: live data panel.
        if (status.isConnected) {
            Spacer(Modifier.height(24.dp))
            ConnectedPanel(status, stats)
        }

        // Failure message: the Go NAT self-check refusal is a long message (with
        // troubleshooting tips and the skip_nat_check hint), so it must be fully
        // readable; hence an expandable card.
        val failure = errorMessage ?: status.lastError.takeIf {
            it.isNotEmpty() && !status.isConnected
        }
        if (failure != null) {
            Spacer(Modifier.height(20.dp))
            FailureCard(failure)
        }

        Spacer(Modifier.height(24.dp))
      } // end of content area; below is pinned to the bottom

        // If the config is incomplete, say what is missing: a disabled button with
        // no explanation just feels broken. The text says which config field to
        // fill in.
        if (!status.isRunning && configIssue != null) {
            Text(
                configIssue,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.fillMaxWidth(),
                textAlign = TextAlign.Center,
            )
            Spacer(Modifier.height(12.dp))
        }

        // Start/Stop plus a gear: the gear sits to the right of the button. Config
        // is a secondary entry that is always available; an icon button does not
        // compete with the main button and does not take a full row like a text
        // button would.
        Row(
            modifier = Modifier.fillMaxWidth(),
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            Box(modifier = Modifier.weight(1f)) {
                ActionButton(
                    status = status,
                    canStart = configIssue == null,
                    onStart = { showIfacePicker = true },
                    onStop = { service.stopTunnel() },
                )
            }
            FilledTonalIconButton(
                onClick = onOpenConfig,
                modifier = Modifier.size(52.dp),
            ) {
                Icon(
                    imageVector = Icons.Filled.Settings,
                    contentDescription = "Settings",
                )
            }
        }
    }

    if (showRoomEditor) {
        TextEditDialog(
            title = "Room name",
            initial = room,
            hint = "Both ends must use the same room name to pair",
            onDismiss = { showRoomEditor = false },
            onConfirm = { value ->
                // Full-width to half-width: the room name must match the peer's
                // exactly, and full-width characters look the same but never match.
                // No character set restriction; the Go side imposes no format.
                val normalized = value.toHalfWidth().trim()
                room = normalized
                val config = runCatching {
                    JSONObject(configStore.currentConfig)
                        .put("room", normalized)
                        .toString(2)
                }.getOrNull()
                if (config != null) {
                    if (role == "server") {
                        configStore.serverConfig = config
                    } else {
                        configStore.clientConfig = config
                    }
                }
            },
        )
    }

    // Pick the interface before starting: the list is enumerated when the dialog opens, so every entry exists.
    if (showIfacePicker) {
        InterfaceDialog(
            initial = "",
            candidates = listUsableInterfaces(),
            onDismiss = { showIfacePicker = false },
            onConfirm = { iface ->
                errorMessage = null
                TunnelService.start(service)
                service.startTunnel(role, configStore.currentConfig, iface) { err ->
                    errorMessage = err
                }
            },
        )
    }
}

/**
 * Checks whether the config is ready to start; returns null if so, otherwise
 * a one-line hint.
 *
 * Only catches obviously unfilled configs: the example signal address from
 * the default template, or empty required fields. Real validation still
 * happens in Mobile.validateConfig at start; this only spares the user from
 * tapping Start on an empty config, hitting an error and concluding the app
 * does not work.
 */
private fun configReadiness(configJson: String, role: String): String? {
    val obj = try {
        JSONObject(configJson)
    } catch (e: Throwable) {
        return "Config is invalid. Check it in Settings."
    }
    val signal = obj.optString("signal")
    if (signal.isBlank()) return "Set the signal server in Settings first."
    // The placeholder address from DEFAULT_CLIENT/DEFAULT_SERVER means it was never configured.
    if (signal.contains("example.com")) return "Set your own signal server in Settings first."

    if (obj.optString("room").isBlank()) return "Set a room name in Settings first."
    // bind_interface is deliberately not checked: it is not stored in the
    // config. The user picks the interface in the dialog shown on Start, and
    // TunnelService.startTunnel injects it into that start's config copy.

    val addrKey = if (role == "server") "forward" else "listen"
    if (obj.optString(addrKey).isBlank()) {
        return if (role == "server") "Set the forward target in Settings first." else "Set the local listen address in Settings first."
    }
    return null
}

/**
 * Role switcher: client / server.
 */
@Composable
private fun RoleSwitcher(role: String, enabled: Boolean, onRoleChange: (String) -> Unit) {
    // Two cards side by side rather than a SegmentedButton, whose default
    // style is cramped here: two lines of text squeezed into a low button,
    // with only a small check mark for the selection. Cards have room for
    // padding, and the selection is shown with a primary container plus a
    // primary outline, so the current role is clear at a glance. While
    // running, the whole row is dimmed to show it cannot be changed.
    val options = listOf(
        Triple("client", "Client", "Local entry for your apps"),
        Triple("server", "Server", "Forwards traffic to a target"),
    )
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .alpha(if (enabled) 1f else 0.55f),
        horizontalArrangement = Arrangement.spacedBy(10.dp),
    ) {
        options.forEach { (value, title, desc) ->
            val selected = role == value
            Surface(
                modifier = Modifier.weight(1f),
                shape = RoundedCornerShape(14.dp),
                color = if (selected) {
                    MaterialTheme.colorScheme.primaryContainer
                } else {
                    MaterialTheme.colorScheme.surfaceContainerLow
                },
                border = if (selected) {
                    androidx.compose.foundation.BorderStroke(
                        1.5.dp,
                        MaterialTheme.colorScheme.primary,
                    )
                } else {
                    null
                },
                onClick = { if (enabled) onRoleChange(value) },
                enabled = enabled,
            ) {
                Column(
                    modifier = Modifier.padding(horizontal = 14.dp, vertical = 12.dp),
                ) {
                    Text(
                        title,
                        style = MaterialTheme.typography.titleSmall,
                        fontWeight = if (selected) FontWeight.SemiBold else FontWeight.Normal,
                        color = if (selected) {
                            MaterialTheme.colorScheme.onPrimaryContainer
                        } else {
                            MaterialTheme.colorScheme.onSurface
                        },
                    )
                    Spacer(Modifier.height(2.dp))
                    Text(
                        desc,
                        style = MaterialTheme.typography.labelSmall,
                        color = if (selected) {
                            MaterialTheme.colorScheme.onPrimaryContainer.copy(alpha = 0.75f)
                        } else {
                            MaterialTheme.colorScheme.onSurfaceVariant
                        },
                    )
                }
            }
        }
    }
}

/**
 * Main status visual: an indicator, one line of status text and one line of
 * explanation.
 *
 * The three kinds of state get clearly different visual weight:
 * - in progress (starting/idle/punching): pulsing halo plus spinning
 *   progress ring, primary color, conveying activity
 * - connected: solid dot in the success color with a one-time pop-in
 * - stopped/failed: static, neutral or error color
 *
 * AnimatedContent transitions between states instead of swapping text.
 */
@Composable
private fun StatusHero(
    status: TunnelStatus,
    role: String,
    room: String,
    onRoomClick: () -> Unit,
) {
    Column(
        modifier = Modifier.fillMaxWidth(),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        // Nothing is drawn while stopped: it would just be a lone empty ring
        // that conveys neither progress nor state. The indicator appears only
        // when there is something to show (the punching pulse, the connected
        // dot). idle is drawn too (static dot): the tunnel really is running,
        // it just has not started punching, and without it the user would think
        // the start failed. isBusy excludes idle, so it takes the static branch
        // and does not spin misleadingly. When stopped, an equal-height spacer
        // keeps the status text from jumping on start/stop.
        if (status.isBusy || status.isConnected || status.isIdle) {
            StatusIndicator(status)
        } else {
            Spacer(Modifier.height(120.dp))
        }
        Spacer(Modifier.height(20.dp))

        // Key only on state: the text only changes with state. Keying on the
        // whole status would run the transition on every per-second poll
        // (uptime and traffic keep changing), causing constant recomposition and
        // flickering text.
        AnimatedContent(
            targetState = status.state,
            transitionSpec = {
                (fadeIn(tween(400)) + scaleIn(tween(400), initialScale = 0.92f))
                    .togetherWith(fadeOut(tween(200)))
            },
            label = "status text",
        ) { _ ->
            Column(horizontalAlignment = Alignment.CenterHorizontally) {
                Text(
                    text = status.displayName,
                    style = MaterialTheme.typography.headlineMedium,
                    fontWeight = FontWeight.SemiBold,
                    color = statusColor(status),
                )
                val hint = status.detailHint(role)
                if (hint.isNotEmpty()) {
                    Spacer(Modifier.height(6.dp))
                    Text(
                        text = hint,
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }

        // The room name is always shown and tappable to edit.
        //
        // It is what pairs the two ends and is needed every time the user sets up
        // the peer, so it comes from the config itself and is visible even while
        // stopped.
        Spacer(Modifier.height(10.dp))
        Surface(
            shape = RoundedCornerShape(20.dp),
            color = MaterialTheme.colorScheme.surfaceContainerHighest,
            onClick = onRoomClick,
            enabled = !status.isRunning,
        ) {
            Text(
                text = if (room.isNotEmpty()) "Room $room" else "Tap to set room name",
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(horizontal = 14.dp, vertical = 6.dp),
            )
        }
    }
}

/** State color: primary while in progress, green when connected, error on failure. */
@Composable
private fun statusColor(status: TunnelStatus): Color = when {
    status.isConnected -> CONNECTED_GREEN
    status.isFailed -> MaterialTheme.colorScheme.error
    status.isBusy -> MaterialTheme.colorScheme.primary
    // idle uses the secondary color: the tunnel is up (brighter than the
    // neutral "Stopped") but not yet connected, so it should not get the same
    // emphasis.
    status.isIdle -> MaterialTheme.colorScheme.secondary
    else -> MaterialTheme.colorScheme.onSurface
}

/**
 * Green for a successful connection.
 *
 * Not colorScheme.primary: "connected" must be distinguishable from "in
 * progress", which already uses primary. Not tertiary either (pinkish
 * purple in the default scheme, which does not read as success). Green is
 * the common mental model for connected and is readable in both light and
 * dark themes.
 */
private val CONNECTED_GREEN = Color(0xFF2E9E5B)

/**
 * Status indicator graphic.
 *
 * In progress: a pulse ring fading inward (looping forever) plus a spinning
 * progress ring, conveying continuous attempts; the birthday strategy runs
 * round after round, which the pulse rhythm mirrors.
 * Connected: a solid dot that pops in once.
 */
@Composable
private fun StatusIndicator(status: TunnelStatus) {
    val color = statusColor(status)
    Box(
        modifier = Modifier.size(120.dp),
        contentAlignment = Alignment.Center,
    ) {
        if (status.isBusy) {
            // Pulse halo: radius grows while opacity fades, looping forever.
            val transition = rememberInfiniteTransition(label = "pulse")
            val pulse by transition.animateFloat(
                initialValue = 0f,
                targetValue = 1f,
                animationSpec = infiniteRepeatable(
                    animation = tween(1800, easing = FastOutSlowInEasing),
                    repeatMode = RepeatMode.Restart,
                ),
                label = "pulse progress",
            )
            Canvas(modifier = Modifier.size(120.dp)) {
                val maxRadius = size.minDimension / 2f
                // Two halos half a cycle apart make the rhythm continuous rather than choppy.
                listOf(pulse, (pulse + 0.5f) % 1f).forEach { p ->
                    drawCircle(
                        color = color.copy(alpha = (1f - p) * 0.28f),
                        radius = maxRadius * (0.45f + p * 0.55f),
                    )
                }
            }
            CircularProgressIndicator(
                modifier = Modifier.size(56.dp),
                color = color,
                strokeWidth = 3.dp,
            )
        } else {
            // Pop in once when connected; other static states have no animation.
            val scale by animateFloatAsState(
                targetValue = 1f,
                animationSpec = tween(500, easing = FastOutSlowInEasing),
                label = "dot scale",
            )
            Canvas(
                modifier = Modifier
                    .size(if (status.isConnected) 56.dp else 40.dp)
                    .scale(if (status.isConnected) scale else 1f),
            ) {
                val r = size.minDimension / 2f
                if (status.isConnected) {
                    drawCircle(color = color.copy(alpha = 0.18f), radius = r)
                    drawCircle(color = color, radius = r * 0.5f)
                } else {
                    drawCircle(
                        color = color.copy(alpha = 0.35f),
                        radius = r * 0.5f,
                        style = androidx.compose.ui.graphics.drawscope.Stroke(width = 3f),
                    )
                }
            }
        }
    }
}

/**
 * Big connect-address block, the centerpiece of the home screen.
 *
 * After starting the client, the only thing the user has to do is point
 * their upstream app at this address, so it must be one of the most
 * prominent elements: monospace, large, on a primary container, with IP and
 * PORT set apart by font weight.
 *
 * 0.0.0.0: showing "0.0.0.0:1080" is meaningless to the user; it is the
 * wildcard for "listen on all interfaces", not an address to enter
 * anywhere. It is shown as 127.0.0.1 with an explanation, since using the
 * tunnel from an app on the same device is by far the most common case.
 */
@Composable
private fun EntryAddressCard(listen: String) {
    val context = LocalContext.current
    var copied by remember { mutableStateOf(false) }

    // Copy feedback resets after 1.5 s. Keyed on copied, so each true restarts the timer.
    LaunchedEffect(copied) {
        if (copied) {
            delay(1500)
            copied = false
        }
    }

    val isWildcard = listen.startsWith("0.0.0.0:") || listen.startsWith("[::]:")
    val display = if (isWildcard) {
        "127.0.0.1:" + listen.substringAfterLast(':')
    } else {
        listen
    }
    val host = display.substringBeforeLast(':', "")
    val port = display.substringAfterLast(':', "")

    Surface(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(20.dp),
        color = MaterialTheme.colorScheme.primaryContainer,
    ) {
        Column(
            modifier = Modifier.padding(20.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Text(
                "Point your app at this address",
                style = MaterialTheme.typography.labelLarge,
                color = MaterialTheme.colorScheme.onPrimaryContainer.copy(alpha = 0.75f),
            )
            Spacer(Modifier.height(12.dp))

            // IP and PORT differ in weight and size: the port is the part users most
            // often misread, so it gets the heavier weight; the colon is a
            // low-opacity separator.
            Row(verticalAlignment = Alignment.Bottom) {
                Text(
                    text = host,
                    fontFamily = FontFamily.Monospace,
                    fontSize = 26.sp,
                    fontWeight = FontWeight.Medium,
                    color = MaterialTheme.colorScheme.onPrimaryContainer,
                )
                Text(
                    text = ":",
                    fontFamily = FontFamily.Monospace,
                    fontSize = 26.sp,
                    color = MaterialTheme.colorScheme.onPrimaryContainer.copy(alpha = 0.45f),
                )
                Text(
                    text = port,
                    fontFamily = FontFamily.Monospace,
                    fontSize = 30.sp,
                    fontWeight = FontWeight.Bold,
                    color = MaterialTheme.colorScheme.onPrimaryContainer,
                )
            }

            if (isWildcard) {
                Spacer(Modifier.height(8.dp))
                Text(
                    "Listening on $listen (all interfaces); use the address above on this device",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onPrimaryContainer.copy(alpha = 0.75f),
                )
            }

            Spacer(Modifier.height(16.dp))
            FilledTonalButton(onClick = {
                copyToClipboard(context, display)
                copied = true
            }) {
                // Icon and text switch with the copy state for immediate feedback.
                AnimatedContent(targetState = copied, label = "copy feedback") { done ->
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Icon(
                            imageVector = if (done) Icons.Filled.Check else Icons.Filled.ContentCopy,
                            contentDescription = null,
                            modifier = Modifier.size(18.dp),
                        )
                        Spacer(Modifier.width(8.dp))
                        Text(if (done) "Copied" else "Copy address")
                    }
                }
            }
        }
    }
}

/**
 * Copies to the clipboard.
 *
 * Android 13 (API 33) and later show their own copy confirmation, so no
 * extra Toast here (it would be a double notice on newer versions). The
 * "Copied" feedback on the button works on all versions.
 */
private fun copyToClipboard(context: Context, text: String) {
    val manager = context.getSystemService(Context.CLIPBOARD_SERVICE) as? ClipboardManager
    manager?.setPrimaryClip(ClipData.newPlainText("burrow", text))
}

/** Waiting card for the server role: says what it is waiting for, without suggesting the user enter forward anywhere. */
@Composable
private fun ServerWaitingCard(status: TunnelStatus) {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(20.dp),
        color = MaterialTheme.colorScheme.surfaceContainerLow,
    ) {
        Column(modifier = Modifier.padding(20.dp)) {
            Text(
                if (status.isConnected) "Client connected" else "Waiting for a client",
                style = MaterialTheme.typography.titleMedium,
                fontWeight = FontWeight.SemiBold,
            )
            Spacer(Modifier.height(6.dp))
            Text(
                "Nothing to enter on this side. Connect from the client with the same room name; " +
                    "traffic is then forwarded to the configured forward target.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

/**
 * Live data panel shown while connected.
 *
 * Information hierarchy: the two most important numbers (live down/up) get a
 * full row in large type; secondary totals and connection counts sit below
 * in a small grid.
 *
 * Values animate with animateFloatAsState instead of jumping every second,
 * since bandwidth refreshes once a second and hard jumps flicker.
 */
@Composable
// No role parameter: the hole count means the same on both ends (one upstream connection = one hole).
private fun ColumnScope.ConnectedPanel(status: TunnelStatus, stats: TunnelStats?) {
    // Speed is derived from the app-level byte counters, not
    // stats.downMbps/upMbps: those come from TCP samples on a single punched
    // socket, and with several fanout candidates on the server they interfere
    // with each other (making server speed read as roughly zero). Remember the
    // previous totals and timestamp and divide the delta by the interval on
    // each update.
    var lastSample by remember { mutableStateOf(Triple(0L, 0L, 0L)) }
    var down by remember { mutableStateOf(0.0) }
    var up by remember { mutableStateOf(0.0) }
    LaunchedEffect(status.appRxBytes, status.appTxBytes) {
        val now = System.currentTimeMillis()
        val (lastRx, lastTx, lastAt) = lastSample
        if (lastAt > 0) {
            val elapsed = (now - lastAt) / 1000.0
            if (elapsed > 0.05) {
                down = (status.appRxBytes - lastRx).coerceAtLeast(0) * 8.0 / elapsed / 1e6
                up = (status.appTxBytes - lastTx).coerceAtLeast(0) * 8.0 / elapsed / 1e6
            }
        }
        lastSample = Triple(status.appRxBytes, status.appTxBytes, now)
    }

    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        SpeedTile("Down", down, status.maxDownMbps, Modifier.weight(1f))
        SpeedTile("Up", up, status.maxUpMbps, Modifier.weight(1f))
    }

    Spacer(Modifier.height(12.dp))

    Surface(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(16.dp),
        color = MaterialTheme.colorScheme.surfaceContainerLow,
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            MetricRow("Uptime", formatUptime(status.uptimeMillis))
            // One upstream connection maps to one hole (one-to-one, no multiplexing),
            // so sessionCount is the number of holes currently open. On the server
            // each hole comes from an upstream connection, summed across clients; on
            // the client it is the number of its own connections in the tunnel.
            MetricRow("Open holes", "${status.sessionCount}")
            // Counted differently from holes, and a mismatch is a useful signal:
            // active connections include those that have not triggered punching yet
            // (lazy punching) and those whose forward target is unreachable, so it is
            // >= the hole count; a persistently larger value means holes are not
            // getting punched.
            MetricRow("Active connections", "${status.activeStreams}")
            MetricRow("Total connections", "${status.totalStreams}")
            // Totals use the app-level counters (AppRx/AppTxBytes), not the TCP
            // sample's TotalRx/TxBytes: those are tied to one punched socket and
            // interfere with each other when the server has several fanout
            // candidates. The app-level counters measure bytes actually relayed,
            // independent of the candidate count.
            MetricRow(
                "Total traffic",
                "↓ ${formatBytes(status.appRxBytes)}  ↑ ${formatBytes(status.appTxBytes)}",
            )
            // TCP quality metrics live here rather than in the log: they refresh every
            // 5 seconds and would flood out the log lines that matter.
            stats?.let {
                MetricRow("RTT", "${it.rttMillis} ms")
                // Retransmission rate is the most direct indicator of punched-path
                // quality, which varies widely; a high rate means a poor hole and says
                // more than RTT alone. Both the current window and the cumulative value
                // are shown: the first reflects now, the second the whole connection.
                MetricRow(
                    "Retransmissions",
                    "%.2f%% (total %.2f%%)".format(it.retransRate, it.totalRetransRate),
                )
                if (it.retransBytes > 0 || it.retransPackets > 0) {
                    MetricRow(
                        "Retransmitted",
                        "${formatBytes(it.retransBytes)} / ${it.retransPackets} pkts",
                    )
                }
                MetricRow("Packets", "↓ ${it.rxPackets}  ↑ ${it.txPackets}")
                if (it.congestionWindow > 0) {
                    MetricRow("Congestion window", formatBytes(it.congestionWindow))
                }
                if (it.receiveWindow > 0) {
                    MetricRow("Receive window", formatBytes(it.receiveWindow))
                }
                // Sample window length, so the user knows what period the values above
                // cover and does not mistake them for instantaneous or cumulative values.
                if (it.intervalMillis > 0) {
                    MetricRow("Sample window", "${it.intervalMillis} ms")
                }
            }
            // Peer list: a server may serve several clients at once, and a count is
            // not enough; the user needs to see who is connected.
            if (status.peers.isNotEmpty()) {
                status.peers.forEachIndexed { index, peer ->
                    MetricRow(if (index == 0) "Peer" else "", peer)
                }
            } else if (status.remoteAddr.isNotEmpty()) {
                MetricRow("Peer", status.remoteAddr)
            }
        }
    }
}

/** A single speed tile: large value, unit and peak. */
@Composable
private fun SpeedTile(label: String, mbps: Double, peak: Double, modifier: Modifier = Modifier) {
    // Animate smoothly so per-second samples do not make the number jump.
    val animated by animateFloatAsState(
        targetValue = mbps.toFloat(),
        animationSpec = tween(800, easing = FastOutSlowInEasing),
        label = "rate",
    )
    Surface(
        modifier = modifier,
        shape = RoundedCornerShape(16.dp),
        color = MaterialTheme.colorScheme.surfaceContainerLow,
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            Text(
                label,
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(Modifier.height(4.dp))
            Row(verticalAlignment = Alignment.Bottom) {
                Text(
                    text = "%.2f".format(animated),
                    style = MaterialTheme.typography.headlineSmall,
                    fontWeight = FontWeight.SemiBold,
                    fontFamily = FontFamily.Monospace,
                )
                Spacer(Modifier.width(4.dp))
                Text(
                    "Mbps",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(bottom = 3.dp),
                )
            }
            Text(
                "Peak %.2f".format(peak),
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

@Composable
private fun MetricRow(label: String, value: String) {
    Row(
        modifier = Modifier.fillMaxWidth().padding(vertical = 5.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            label,
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(
            value,
            style = MaterialTheme.typography.bodyMedium,
            fontFamily = FontFamily.Monospace,
        )
    }
}

/**
 * Failure message card.
 *
 * The Go NAT self-check refusal is a long message (with troubleshooting tips
 * and the skip_nat_check hint) that does not fit on one line. It is
 * collapsed to two lines by default and expands on tap, so long text does
 * not take over the screen but the full guidance is still readable.
 */
@Composable
private fun FailureCard(message: String) {
    var expanded by remember { mutableStateOf(false) }
    Surface(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(16.dp),
        color = MaterialTheme.colorScheme.errorContainer,
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            Text(
                "Start failed",
                style = MaterialTheme.typography.titleSmall,
                fontWeight = FontWeight.SemiBold,
                color = MaterialTheme.colorScheme.onErrorContainer,
            )
            Spacer(Modifier.height(6.dp))
            Text(
                text = message,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onErrorContainer,
                maxLines = if (expanded) Int.MAX_VALUE else 2,
            )
            // Only offer expansion when the text may actually be truncated; short errors need no extra button.
            if (message.length > 60) {
                TextButton(
                    onClick = { expanded = !expanded },
                    colors = ButtonDefaults.textButtonColors(
                        contentColor = MaterialTheme.colorScheme.onErrorContainer,
                    ),
                ) {
                    Text(if (expanded) "Show less" else "Show full reason")
                }
            }
        }
    }
}

/**
 * Main action button.
 *
 * While running (including the start window) it always shows an enabled
 * Stop: the user must be able to interrupt at any time, especially during
 * the server's NAT self-check, which can take up to 30 seconds.
 */
@Composable
private fun ActionButton(
    status: TunnelStatus,
    canStart: Boolean,
    onStart: () -> Unit,
    onStop: () -> Unit,
) {
    AnimatedContent(
        targetState = status.isRunning,
        transitionSpec = { fadeIn(tween(250)).togetherWith(fadeOut(tween(250))) },
        label = "main button",
    ) { running ->
        if (running) {
            OutlinedButton(
                onClick = onStop,
                modifier = Modifier.fillMaxWidth().height(52.dp),
            ) { Text("Stop", style = MaterialTheme.typography.titleMedium) }
        } else {
            // Disabled until the config is complete: the default signal address is a
            // placeholder, and tapping Start would only hit a Go error while implying
            // the app is ready to use. The disabled button plus the hint text above
            // explain what is missing.
            Button(
                onClick = onStart,
                enabled = canStart,
                modifier = Modifier.fillMaxWidth().height(52.dp),
            ) { Text("Start", style = MaterialTheme.typography.titleMedium) }
        }
    }
}
