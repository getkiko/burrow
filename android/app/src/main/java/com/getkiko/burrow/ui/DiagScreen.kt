package com.getkiko.burrow.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExposedDropdownMenuBox
import androidx.compose.material3.ExposedDropdownMenuDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.MenuAnchorType
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TextField
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import com.getkiko.burrow.ConfigStore
import com.getkiko.burrow.listUsableInterfaces
import com.getkiko.burrow.mobile.Mobile
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject

/** Proxy address: allows letters, digits and . : / - _ (so http://10.0.0.200:80 works). */
private fun sanitizeProxyInput(input: String): String =
    input.toHalfWidth().filter {
        it.isLetterOrDigit() || it == '.' || it == ':' || it == '/' || it == '-' || it == '_'
    }

/**
 * Debug screen: the CLI's diagnostic tools (`burrow stun` and
 * `burrow tcpnat`), with the same fields and defaults as the CLI flags.
 *
 * Both diagnostics block for seconds to tens of seconds (STUN defaults to a
 * 30 s timeout; the multi-round, multi-port TCP NAT probe is slower), so they
 * must run on Dispatchers.IO, never on the main thread: the blocking Go call
 * is wrapped in a coroutine and the UI thread only starts it and shows the
 * result.
 *
 * Cancellation: the scope comes from rememberCoroutineScope(), tied to this
 * composable's composition, so coroutines are cancelled automatically when
 * DiagScreen leaves the composition (tab switch, Activity destroyed, ...).
 * While running, the button turns into Cancel so the user can cancel the job
 * instead of stacking up repeated requests.
 *
 * Note that the timeout/port/round parameters of Mobile.runSTUN/runTCPNAT
 * are Java long, not int (gomobile binds Go int as Java long).
 *
 * Text field style matches ConfigScreen: filled TextFields grouped with
 * spacing and small titles, not a Card around each tool.
 */
@Composable
fun DiagScreen(configStore: ConfigStore, modifier: Modifier = Modifier) {
    Column(
        modifier = modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(16.dp),
    ) {
        Text("Debug tools", style = MaterialTheme.typography.headlineSmall)
        Text(
            "STUN and TCP NAT checks, as in the CLI, to find out why punching fails.",
            style = MaterialTheme.typography.bodySmall,
        )
        Spacer(Modifier.height(16.dp))
        StunSection(configStore)
        Spacer(Modifier.height(24.dp))
        TcpNatSection(configStore)
    }
}

/**
 * STUN NAT type detection. Fields and defaults match the `burrow stun` CLI
 * flags: the server defaults to diag.DefaultSTUNServer
 * ("stun.miwifi.com:3478") and the timeout to 30 s (Mobile.runSTUN takes
 * milliseconds, hence 30000).
 *
 * The interface is presented as required (as on the config screen): netlink
 * is restricted on Android, so automatic route selection is unreliable and
 * the UI should not suggest it can be left empty.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun StunSection(configStore: ConfigStore) {
    var server by remember { mutableStateOf(configStore.diagInput("stun_server", "stun.miwifi.com:3478")) }
    var iface by remember { mutableStateOf("") }
    var timeout by remember { mutableStateOf(configStore.diagInput("stun_timeout", "30000")) }
    var result by remember { mutableStateOf<DiagResult?>(null) }
    var running by remember { mutableStateOf(false) }
    var job by remember { mutableStateOf<Job?>(null) }
    val scope = rememberCoroutineScope()

    Column(modifier = Modifier.fillMaxWidth()) {
        Text("STUN check", style = MaterialTheme.typography.titleMedium)
        Text(
            "Detects this device's NAT type. A symmetric NAT (NAT4) cannot be the server with the birthday strategy.",
            style = MaterialTheme.typography.bodySmall,
        )
        Spacer(Modifier.height(8.dp))

        DiagField("STUN server", server) { server = it; configStore.setDiagInput("stun_server", it) }
        InterfaceDropdown(iface) { iface = it }
        DiagField(
            "Timeout (ms, default 30000)",
            timeout,
            keyboardType = KeyboardType.Number,
        ) { timeout = it; configStore.setDiagInput("stun_timeout", it) }

        Spacer(Modifier.height(8.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(
                onClick = {
                    running = true
                    result = null
                    job = scope.launch {
                        // STUN defaults to a 30 s timeout, so run it on the IO thread or
                        // risk an ANR. Exceptions are not written to Compose state from
                        // the IO thread: they become a Result<String>, which is read back
                        // on the main thread (launch runs on Dispatchers.Main.immediate)
                        // after withContext returns.
                        val outcome = withContext(Dispatchers.IO) {
                            try {
                                Result.success(
                                    Mobile.runSTUN(
                                        server,
                                        iface,
                                        timeout.toLongOrNull() ?: 0L,
                                    ),
                                )
                            } catch (e: CancellationException) {
                                // Cancellation is not an error and must not be shown as a
                                // failure: rethrow so the coroutine ends cancelled instead
                                // of falling into catch(Throwable) below.
                                throw e
                            } catch (e: Throwable) {
                                Result.failure(e)
                            }
                        }
                        // If Cancel is tapped after withContext returns but before
                        // this code finishes, job.cancel() is cooperative and does not
                        // affect code without suspension points, so the stale result
                        // would overwrite "Canceled". ensureActive() throws
                        // CancellationException when cancelled, skipping the writes.
                        ensureActive()
                        result = outcome.fold(
                            onSuccess = { parseStunResult(it) },
                            onFailure = { DiagResult.Failure("Check failed: ${it.message}") },
                        )
                        running = false
                        job = null
                    }
                },
                enabled = !running,
                modifier = Modifier.weight(1f),
            ) {
                if (running) {
                    // size rather than height: constraining only the height lets
                    // the width fill the available space and stretches the ring
                    // into a large ellipse.
                    CircularProgressIndicator(
                        modifier = Modifier.size(18.dp),
                        strokeWidth = 2.dp,
                    )
                    Spacer(Modifier.width(8.dp))
                }
                Text(if (running) "Checking (may take tens of seconds)..." else "Start check")
            }
            if (running) {
                TextButton(onClick = {
                    job?.cancel()
                    job = null
                    running = false
                    result = DiagResult.Failure("Canceled")
                }) { Text("Cancel") }
            }
        }

        result?.let {
            Spacer(Modifier.height(8.dp))
            DiagResultView(it)
        }
    }
}

/**
 * TCP NAT mapping behavior probe. Fields and defaults match the
 * `burrow tcpnat` CLI flags: the server (signal address) is required, ports
 * default to "9960,9962,9963", local port to 0 (a new one each round), rounds
 * to 3 and the timeout to 8 s (8000 ms here).
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TcpNatSection(configStore: ConfigStore) {
    var server by remember { mutableStateOf(configStore.diagInput("tcpnat_server", "")) }
    var ports by remember { mutableStateOf(configStore.diagInput("tcpnat_ports", "9960,9962,9963")) }
    var iface by remember { mutableStateOf("") }
    var localPort by remember { mutableStateOf(configStore.diagInput("tcpnat_localport", "0")) }
    var rounds by remember { mutableStateOf(configStore.diagInput("tcpnat_rounds", "3")) }
    var timeout by remember { mutableStateOf(configStore.diagInput("tcpnat_timeout", "8000")) }
    var proxy by remember { mutableStateOf(configStore.diagInput("tcpnat_proxy", "")) }
    var result by remember { mutableStateOf<DiagResult?>(null) }
    var running by remember { mutableStateOf(false) }
    var job by remember { mutableStateOf<Job?>(null) }
    val scope = rememberCoroutineScope()

    Column(modifier = Modifier.fillMaxWidth()) {
        Text("TCP NAT probe", style = MaterialTheme.typography.titleMedium)
        Text(
            "Probes TCP port mapping over several rounds and ports. Slower than STUN.",
            style = MaterialTheme.typography.bodySmall,
        )
        Spacer(Modifier.height(8.dp))

        // The server (signal address) is required. isError flags it without
        // blocking input; the actual required check happens when Start is tapped.
        TextField(
            colors = appTextFieldColors(),
            value = server,
            // Address field: full-width to half-width, and drop spaces/non-ASCII, same rules as the config screen.
            onValueChange = { input ->
                server = input.toHalfWidth().filter { it.code in 0x21..0x7E }
                configStore.setDiagInput("tcpnat_server", server)
            },
            label = { Text("Signal server (required)") },
            placeholder = { Text("ws://ip:port/path") },
            singleLine = true,
            isError = server.isBlank(),
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
            modifier = Modifier.fillMaxWidth(),
        )
        Spacer(Modifier.height(8.dp))
        DiagField("Probe ports (comma-separated, at least two)", ports) { ports = it; configStore.setDiagInput("tcpnat_ports", it) }
        // HTTP proxy: networks such as WAP access points have no direct internet
        // egress, and every outbound connection must go through the gateway proxy
        // (CONNECT). Leave empty to connect directly.
        DiagField(
            "HTTP proxy (optional, e.g. 10.0.0.200:80)",
            proxy,
            keyboardType = KeyboardType.Uri,
        ) { proxy = sanitizeProxyInput(it); configStore.setDiagInput("tcpnat_proxy", proxy) }
        InterfaceDropdown(iface) { iface = it }
        DiagField(
            "Local port (0 = new port each round)",
            localPort,
            keyboardType = KeyboardType.Number,
        ) { localPort = it; configStore.setDiagInput("tcpnat_localport", it) }
        DiagField(
            "Rounds (at least 1, default 3)",
            rounds,
            keyboardType = KeyboardType.Number,
        ) { rounds = it; configStore.setDiagInput("tcpnat_rounds", it) }
        DiagField(
            "Timeout (ms, default 8000)",
            timeout,
            keyboardType = KeyboardType.Number,
        ) { timeout = it; configStore.setDiagInput("tcpnat_timeout", it) }

        Spacer(Modifier.height(8.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(
                onClick = {
                    if (server.isBlank()) {
                        result = DiagResult.Failure("Signal server is required")
                        return@Button
                    }
                    running = true
                    result = null
                    job = scope.launch {
                        // Same as the STUN section: exceptions are not written to result
                        // from the IO thread; they become a Result<String> that is read
                        // back on the main thread after withContext.
                        val outcome = withContext(Dispatchers.IO) {
                            try {
                                Result.success(
                                    Mobile.runTCPNAT(
                                        server,
                                        ports,
                                        iface,
                                        proxy,
                                        localPort.toLongOrNull() ?: 0L,
                                        // rounds <= 0 is rejected by the binding layer
                                        // (unlike timeout, where it means "use the
                                        // default"; see mobile/diag.go), so fall back to
                                        // 3, the CLI default.
                                        rounds.toLongOrNull()?.takeIf { it > 0 } ?: 3L,
                                        timeout.toLongOrNull() ?: 0L,
                                    ),
                                )
                            } catch (e: CancellationException) {
                                // Cancellation is not an error; rethrow.
                                throw e
                            } catch (e: Throwable) {
                                Result.failure(e)
                            }
                        }
                        // If Cancel is tapped after withContext returns but before
                        // this code finishes, job.cancel() is cooperative and does
                        // not affect code without suspension points, so the stale
                        // result would overwrite "Canceled". ensureActive() throws
                        // CancellationException when cancelled, skipping the writes.
                        ensureActive()
                        result = outcome.fold(
                            onSuccess = { parseTcpNatResult(it) },
                            onFailure = { DiagResult.Failure("Probe failed: ${it.message}") },
                        )
                        running = false
                        job = null
                    }
                },
                enabled = !running,
                modifier = Modifier.weight(1f),
            ) {
                if (running) {
                    // size rather than height: constraining only the height lets
                    // the width fill the available space and stretches the ring
                    // into a large ellipse.
                    CircularProgressIndicator(
                        modifier = Modifier.size(18.dp),
                        strokeWidth = 2.dp,
                    )
                    Spacer(Modifier.width(8.dp))
                }
                Text(if (running) "Probing (may take a while)..." else "Start probe")
            }
            if (running) {
                TextButton(onClick = {
                    job?.cancel()
                    job = null
                    running = false
                    result = DiagResult.Failure("Canceled")
                }) { Text("Cancel") }
            }
        }

        result?.let {
            Spacer(Modifier.height(8.dp))
            DiagResultView(it)
        }
    }
}

/**
 * Outbound interface dropdown, reusing listUsableInterfaces().
 *
 * It does not offer "leave empty for automatic": netlink is restricted on
 * Android, so automatic route selection is unreliable (which is why the Go
 * side makes bind_interface mandatory). This keeps the same required
 * semantics as the config screen.
 *
 * Only enumerated interfaces can be picked, no manual entry: interface names
 * are not something users can make up, and a name that does not exist on
 * this device only makes the probe fail.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun InterfaceDropdown(value: String, onChange: (String) -> Unit) {
    var expanded by remember { mutableStateOf(false) }
    // Re-enumerate on every expand, no caching: the user may lose the network,
    // switch Wi-Fi/mobile data or toggle airplane mode while on this screen, and
    // a cached list could offer an interface that no longer exists, making the
    // probe fail in a way that is hard to diagnose.
    var interfaces by remember { mutableStateOf(emptyList<String>()) }
    ExposedDropdownMenuBox(
        expanded = expanded,
        onExpandedChange = {
            if (it) interfaces = listUsableInterfaces()
            expanded = it
        },
    ) {
        TextField(
            colors = appTextFieldColors(),
            value = value,
            onValueChange = {},
            readOnly = true,
            label = { Text("Network interface") },
            placeholder = { Text("Tap to choose") },
            singleLine = true,
            trailingIcon = {
                ExposedDropdownMenuDefaults.TrailingIcon(expanded = expanded)
            },
            modifier = Modifier
                .fillMaxWidth()
                .menuAnchor(MenuAnchorType.PrimaryNotEditable),
        )
        ExposedDropdownMenu(
            expanded = expanded,
            onDismissRequest = { expanded = false },
        ) {
            if (interfaces.isEmpty()) {
                DropdownMenuItem(
                    text = { Text("No usable interface found. Check your network connection.") },
                    onClick = { expanded = false },
                )
            }
            interfaces.forEach { name ->
                DropdownMenuItem(
                    text = { Text(name) },
                    onClick = {
                        onChange(name)
                        expanded = false
                    },
                )
            }
        }
    }
}

/** Diagnostic result: structured fields (shown prominently) plus the full Detail text (secondary, scrollable). */
private sealed interface DiagResult {
    data class Stun(
        val natLabel: String,
        val publicAddr: String,
        val isSymmetric: Boolean,
        val detail: String,
    ) : DiagResult

    data class TcpNat(val detail: String) : DiagResult

    data class Failure(val message: String) : DiagResult
}

private fun parseStunResult(json: String): DiagResult =
    try {
        val obj = JSONObject(json)
        DiagResult.Stun(
            natLabel = obj.optString("NATLabel").ifEmpty { obj.optString("NATType") },
            publicAddr = obj.optString("PublicAddr"),
            isSymmetric = obj.optBoolean("IsSymmetric"),
            detail = obj.optString("Detail"),
        )
    } catch (e: Throwable) {
        DiagResult.Failure("Failed to parse result: ${e.message}\n$json")
    }

private fun parseTcpNatResult(json: String): DiagResult =
    try {
        DiagResult.TcpNat(detail = JSONObject(json).optString("Detail", json))
    } catch (e: Throwable) {
        DiagResult.Failure("Failed to parse result: ${e.message}\n$json")
    }

@Composable
private fun DiagResultView(result: DiagResult) {
    when (result) {
        is DiagResult.Stun -> {
            Column {
                if (result.isSymmetric) {
                    // With the birthday strategy, the server refuses to start behind a
                    // symmetric NAT (NAT4) (see the tunnel package's NAT check). Tell the
                    // user here, before a real start fails confusingly; the wording
                    // echoes that error.
                    //
                    // Surface (a plain color block, no outline) rather than Card: this is
                    // an informative warning block, not a grouping container.
                    Surface(
                        modifier = Modifier.fillMaxWidth(),
                        color = MaterialTheme.colorScheme.errorContainer,
                    ) {
                        Column(modifier = Modifier.padding(12.dp)) {
                            Text(
                                "This device is behind a symmetric NAT (NAT4)",
                                style = MaterialTheme.typography.titleSmall,
                                color = MaterialTheme.colorScheme.onErrorContainer,
                            )
                            Text(
                                "The birthday strategy needs a stable port mapping on the server. " +
                                    "A symmetric NAT assigns a new public port for every new destination, " +
                                    "so this device cannot be the server with the birthday strategy.",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onErrorContainer,
                            )
                        }
                    }
                    Spacer(Modifier.height(8.dp))
                }
                if (result.natLabel.isNotEmpty()) {
                    ResultRow("NAT type", result.natLabel)
                }
                if (result.publicAddr.isNotEmpty()) {
                    ResultRow("Public mapping", result.publicAddr)
                }
                Spacer(Modifier.height(8.dp))
                DetailBox(result.detail)
            }
        }
        is DiagResult.TcpNat -> DetailBox(result.detail)
        is DiagResult.Failure -> Text(
            result.message,
            color = MaterialTheme.colorScheme.error,
            style = MaterialTheme.typography.bodySmall,
        )
    }
}

@Composable
private fun ResultRow(label: String, value: String) {
    Row(modifier = Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
        Text(label, style = MaterialTheme.typography.bodyMedium)
        Text(
            value,
            style = MaterialTheme.typography.bodyMedium,
            fontFamily = FontFamily.Monospace,
        )
    }
}

/** The full Detail text, collapsed below the structured fields so long output does not fill the screen. */
@Composable
private fun DetailBox(detail: String) {
    var expanded by remember { mutableStateOf(false) }
    TextButton(onClick = { expanded = !expanded }) {
        Text(if (expanded) "Hide full output" else "Show full output")
    }
    if (expanded) {
        TextField(
            colors = appTextFieldColors(),
            value = detail,
            onValueChange = {},
            readOnly = true,
            modifier = Modifier.fillMaxWidth().height(240.dp),
            textStyle = MaterialTheme.typography.bodySmall.copy(fontFamily = FontFamily.Monospace),
        )
    }
}

/**
 * Text field for the debug screen.
 *
 * Input filtering lives here rather than at every call site:
 * - Number fields accept digits only; the keyboard type does not stop paste
 *   or hardware keyboards.
 * - Other fields convert full-width to half-width, catching the full-width
 *   colon a CJK input method slips into addresses like "127.0.0.1:8080".
 */
@Composable
private fun DiagField(
    label: String,
    value: String,
    keyboardType: KeyboardType = KeyboardType.Text,
    onChange: (String) -> Unit,
) {
    TextField(
        colors = appTextFieldColors(),
        value = value,
        onValueChange = { input ->
            onChange(
                if (keyboardType == KeyboardType.Number) {
                    input.filter { it.isDigit() }
                } else {
                    input.toHalfWidth()
                },
            )
        },
        label = { Text(label) },
        singleLine = true,
        keyboardOptions = KeyboardOptions(keyboardType = keyboardType),
        modifier = Modifier.fillMaxWidth(),
    )
}
