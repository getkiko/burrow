package com.getkiko.burrow.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Check
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Icon
import androidx.compose.material3.TextButton
import androidx.compose.material3.IconButton
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextField
import androidx.compose.material3.TopAppBar
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import com.getkiko.burrow.ConfigStore
import com.getkiko.burrow.listUsableInterfaces
import com.getkiko.burrow.mobile.Mobile
import org.json.JSONObject
import java.net.URI

/**
 * Removes the redundant backslashes org.json adds before forward slashes.
 *
 * In JSON (RFC 8259) "/" is an ordinary character that needs no escaping;
 * "\/" is legal but org.json writes every "/" as "\/" regardless. The signal
 * address then shows up in the preview as "ws:\/\/host:9960\/path", which
 * looks garbled.
 *
 * Only "\/" -> "/" is restored; other escapes are untouched. A literal
 * backslash in a JSON string is written "\\", so in "\\/" (escaped backslash
 * followed by a slash) the "\/" is not an escape pair and must not be
 * replaced; the regex requires that the preceding character is not a
 * backslash.
 */
private val REDUNDANT_SLASH_ESCAPE = Regex("""(?<!\\)\\/""")

private fun String.unescapeSlashes(): String =
    REDUNDANT_SLASH_ESCAPE.replace(this, "/")

/**
 * Extracts the host from the signal field (e.g.
 * "ws://signal.example.com:9960/path").
 *
 * Parses with java.net.URI rather than splitting strings by hand: scheme,
 * port, path and IPv6 literals (e.g. "ws://[::1]:9960/") are all URI syntax
 * and easy to get wrong with a hand-written regex. An IPv6 literal keeps its
 * brackets in URI.host (e.g. "[::1]"); isLikelyIP() strips them before
 * checking.
 *
 * Returns null if signal is empty, malformed or has no host. Callers should
 * then keep the signal IP input disabled (no hosts mapping) rather than
 * crash or write garbage.
 */
private fun parseSignalHost(signal: String): String? {
    if (signal.isBlank()) return null
    return try {
        val uri = URI(signal.trim())
        uri.host?.takeIf { it.isNotBlank() }
    } catch (e: Throwable) {
        null
    }
}

/**
 * Tells whether a host is an IP literal or a domain name.
 *
 * The signal IP input only makes sense for a domain name: if signal already
 * contains an IP (e.g. "ws://1.2.3.4:9960/path" or an IPv6 literal), there is
 * no name to map, and an "IP -> IP" hosts entry would be meaningless.
 */
private fun isLikelyIP(host: String): Boolean {
    val bare = host.removePrefix("[").removeSuffix("]")
    // Validating with InetAddresses would need an extra dependency, so use a
    // pragmatic heuristic: only digits and dots (IPv4), or containing a colon
    // (the hallmark of an IPv6 literal; domain names cannot contain colons).
    if (bare.contains(":")) return true
    return bare.isNotEmpty() && bare.all { it.isDigit() || it == '.' }
}

/**
 * Reads the IP mapped to the signal host in hosts, to prefill the signal IP
 * input.
 *
 * Returns an empty string if there is no entry (never set, or the signal
 * host changed so the key no longer matches) rather than throwing or showing
 * an old value: the input should honestly reflect whether the current signal
 * host has a hosts mapping.
 */
private fun readSignalIP(obj: JSONObject, signalHost: String?): String {
    if (signalHost.isNullOrBlank()) return ""
    val hosts = obj.optJSONObject("hosts") ?: return ""
    return hosts.optString(signalHost)
}

/**
 * Syncs the signal IP input into the hosts map. Only the key for the signal
 * host is managed; other hosts entries written in the advanced JSON editor
 * are kept as is.
 *
 * Edge cases:
 * - ip is empty (input cleared): remove the signalHost entry; if hosts is
 *   then empty, remove the "hosts" key itself instead of leaving
 *   "hosts": {} in the preview.
 * - signalHost changed (the user edited the host in signal): callers pass
 *   the new signalHost and only the new key is written. The orphan entry
 *   for the old host is cleaned up by updateSignalField() when signal
 *   changes; that is not this function's job.
 * - signalHost is null (signal empty, invalid or an IP): skip without
 *   writing anything; callers should not let the user edit the input in
 *   that state anyway.
 */
private fun applySignalIP(obj: JSONObject, signalHost: String?, ip: String) {
    if (signalHost.isNullOrBlank()) return
    val hosts = obj.optJSONObject("hosts") ?: JSONObject()
    if (ip.isBlank()) {
        hosts.remove(signalHost)
    } else {
        hosts.put(signalHost, ip)
    }
    if (hosts.length() == 0) {
        obj.remove("hosts")
    } else {
        obj.put("hosts", hosts)
    }
}

/**
 * Filter for address inputs (listen / forward): convert to half-width, then
 * drop spaces and non-ASCII.
 *
 * Non-ASCII characters or spaces in an address are always typos; that is a
 * format issue that needs no knowledge of Go-side rules, so it is blocked
 * while typing.
 */
private fun sanitizeAddr(input: String): String =
    input.toHalfWidth().filter { it.code in 0x21..0x7E }

/** A signal IP consists only of digits, dots (IPv4) and colons (IPv6). */
private fun sanitizeIP(input: String): String =
    input.toHalfWidth().filter { it.isDigit() || it == '.' || it == ':' }

/** Digits only (ports, rounds, concurrency, ...). */
private fun sanitizeDigits(input: String): String =
    input.toHalfWidth().filter { it.isDigit() }

/** Interface name: letters, digits and a few separators (e.g. wlan0, rmnet_data0); no spaces or non-ASCII. */
private fun sanitizeIface(input: String): String =
    input.toHalfWidth().filter { it.isLetterOrDigit() || it == '_' || it == '-' || it == '.' }

/**
 * Checks a "host:port" address; returns an error description, or null if it
 * passes.
 *
 * Format checks only, for things that are wrong regardless of Go-side
 * logic: missing colon, non-numeric port, port out of range. Whether the
 * address can actually be bound, or is valid in combination with other
 * fields, is left to Mobile.validateConfig.
 */
private fun checkHostPort(value: String, label: String): String? {
    val idx = value.lastIndexOf(':')
    if (idx <= 0 || idx == value.length - 1) {
        return "$label must be host:port, e.g. 127.0.0.1:1080"
    }
    val port = value.substring(idx + 1).toIntOrNull()
        ?: return "$label: port must be a number"
    if (port !in 1..65535) {
        return "$label: port must be between 1 and 65535"
    }
    // The host must be checked too: validating only the port would let an
    // obviously wrong address such as "127.0.01:8080" (a missing dot) through.
    val host = value.substring(0, idx)
    return checkHost(host, label)
}

/**
 * Checks the host part of an address: an IPv4 literal, a bracketed IPv6
 * literal or a domain name.
 *
 * Only rejects things that are not a host at all; semantics (is the IP
 * reachable, does the name resolve) are left to the Go side.
 */
private fun checkHost(host: String, label: String): String? {
    if (host.isBlank()) {
        return "$label is missing the host, e.g. 127.0.0.1:1080"
    }
    // IPv6 literal, as in [::1]:8080.
    if (host.startsWith("[")) {
        if (!host.endsWith("]") || host.length <= 2) {
            return "$label: invalid IPv6 address, e.g. [::1]:1080"
        }
        return null
    }
    // Only digits and dots: the user meant IPv4, so check it strictly as IPv4.
    // That way "127.0.01", "127.0.0" or "999.1.1.1" get a precise error instead
    // of passing as a domain name.
    if (host.all { it.isDigit() || it == '.' }) {
        val parts = host.split('.')
        if (parts.size != 4 || parts.any { p -> p.isEmpty() || p.toIntOrNull()?.let { it !in 0..255 } != false }) {
            return "$label: invalid IP address $host (four numbers 0-255, e.g. 127.0.0.1)"
        }
        return null
    }
    // Everything else is a domain name: letters, digits, hyphens and dots, not starting or ending with a dot or hyphen.
    val domainOk = host.all { it.isLetterOrDigit() || it == '-' || it == '.' } &&
        !host.startsWith('.') && !host.endsWith('.') &&
        !host.startsWith('-') && !host.endsWith('-')
    if (!domainOk) {
        return "$label: invalid characters in $host"
    }
    return null
}

/**
 * Combines the signal address and signal IP into one display line.
 *
 * The IP is added after the host in angle brackets:
 *   ws://signal.example.com:8900/abc  +  203.0.113.10
 *   -> ws://signal.example.com<203.0.113.10>:8900/abc
 *
 * This shows at a glance which host is used and which IP it resolves to.
 * Without an IP the address is shown unchanged.
 */
private fun formatSignalDisplay(signal: String, ip: String): String {
    if (signal.isBlank()) return ""
    if (ip.isBlank()) return signal
    val host = parseSignalHost(signal) ?: return signal
    // Replace only the host, keeping scheme/port/path: a plain string replace
    // could hit a matching segment in the path, so limit it to the first
    // occurrence after "//host".
    val marker = "//$host"
    val idx = signal.indexOf(marker)
    if (idx < 0) return signal
    val cut = idx + marker.length
    return signal.substring(0, cut) + "<$ip>" + signal.substring(cut)
}

/**
 * Checks the signal address: it must have a scheme and a host, or the signal
 * server cannot be reached.
 *
 * Shared by the as-you-type check and localCheck() on save, so both use the
 * same rules and the input never passes while saving fails.
 */
private fun checkSignalUri(value: String): String? {
    val uri = try {
        URI(value.trim())
    } catch (e: Throwable) {
        null
    }
    if (uri?.scheme.isNullOrBlank() || uri?.host.isNullOrBlank()) {
        return "Invalid signal address, e.g. ws://example.com:9960/path"
    }
    return null
}

/** Checks an IPv4 literal; only called when the user entered a signal IP. */
private fun checkIPv4(value: String): String? {
    val parts = value.split('.')
    if (parts.size != 4 || parts.any { p -> p.isEmpty() || p.toIntOrNull()?.let { it !in 0..255 } != false }) {
        return "Signal IP must be a valid IPv4 address, e.g. 1.2.3.4"
    }
    return null
}

/** The open dialog. A sealed class instead of a dozen boolean states guarantees at most one is open. */
private sealed interface ConfigDialog {
    data object Signal : ConfigDialog
    data object Trials : ConfigDialog
    data object Strategy : ConfigDialog
    data object LogLevel : ConfigDialog
    data object MaxConcurrent : ConfigDialog
    data object Birthday : ConfigDialog
    data object Predictive : ConfigDialog
    data object RawJson : ConfigDialog
}

/**
 * Signal dialog: address plus an optional IP.
 *
 * The IP field only appears when the address contains a domain name: if it
 * is already an IP there is no name to map, and an "IP -> IP" hosts entry
 * would be meaningless. This follows the input live: changing the host to
 * an IP hides the IP field immediately.
 *
 * Both values are written back together on OK; until then they live in the
 * dialog's own state and Cancel discards them.
 */
@Composable
private fun SignalDialog(
    initialSignal: String,
    initialIP: String,
    onDismiss: () -> Unit,
    onConfirm: (signal: String, ip: String) -> Unit,
) {
    var signal by remember { mutableStateOf(initialSignal) }
    var ip by remember { mutableStateOf(initialIP) }

    val host = parseSignalHost(signal)
    val hostIsIP = host != null && isLikelyIP(host)
    val showIPField = host != null && !hostIsIP

    val signalError = if (signal.isBlank()) null else checkSignalUri(signal)
    val ipError = if (ip.isBlank() || !showIPField) null else checkIPv4(ip)
    val canConfirm = signal.isNotBlank() && signalError == null && ipError == null

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Signal server") },
        text = {
            Column {
                TextField(
                    colors = appTextFieldColors(),
                    value = signal,
                    onValueChange = { signal = sanitizeAddr(it) },
                    label = { Text("Address") },
                    placeholder = { Text("ws://example.com:9960/path") },
                    singleLine = true,
                    isError = signalError != null,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
                    modifier = Modifier.fillMaxWidth(),
                )
                if (signalError != null) {
                    Text(
                        signalError,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                        modifier = Modifier.padding(start = 16.dp, top = 4.dp),
                    )
                }

                if (showIPField) {
                    Spacer(Modifier.height(12.dp))
                    TextField(
                        colors = appTextFieldColors(),
                        value = ip,
                        onValueChange = { ip = sanitizeIP(it) },
                        label = { Text("Signal IP (optional)") },
                        placeholder = { Text("e.g. 203.0.113.10") },
                        singleLine = true,
                        isError = ipError != null,
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                        modifier = Modifier.fillMaxWidth(),
                    )
                    Text(
                        ipError ?: "IP for the host above, added to hosts to bypass system DNS. Leave empty to skip.",
                        style = MaterialTheme.typography.bodySmall,
                        color = if (ipError != null) {
                            MaterialTheme.colorScheme.error
                        } else {
                            MaterialTheme.colorScheme.onSurfaceVariant
                        },
                        modifier = Modifier.padding(start = 16.dp, top = 4.dp),
                    )
                } else if (hostIsIP) {
                    Spacer(Modifier.height(8.dp))
                    Text(
                        "The address is already an IP; no signal IP needed",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        },
        confirmButton = {
            TextButton(
                onClick = {
                    // If the address is an IP, clear the IP field so no orphan hosts entry is left.
                    onConfirm(signal, if (showIPField) ip else "")
                    onDismiss()
                },
                enabled = canConfirm,
            ) { Text("OK") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/** One-line summary of the birthday parameters, computed from the current values. */
private fun birthdaySummary(b: JSONObject): String {
    val parts = mutableListOf<String>()
    b.optInt("candidates", 0).takeIf { it > 0 }?.let { parts += "$it candidates" }
    val min = b.optInt("min_port", 0)
    val max = b.optInt("max_port", 0)
    if (min > 0 && max > 0) parts += "ports $min-$max"
    b.optString("selection").takeIf { it.isNotBlank() }?.let { parts += it }
    b.optString("attempt_timeout").takeIf { it.isNotBlank() }?.let { parts += "timeout $it" }
    return parts.joinToString(" • ").ifBlank { "Defaults" }
}

/** One-line summary of the predictive parameters. */
private fun predictiveSummary(p: JSONObject): String {
    val parts = mutableListOf<String>()
    p.optInt("port_scan_range", 0).takeIf { it > 0 }?.let { parts += "scan range $it" }
    p.optInt("concurrency", 0).takeIf { it > 0 }?.let { parts += "concurrency $it" }
    p.optString("attempt_timeout").takeIf { it.isNotBlank() }?.let { parts += "timeout $it" }
    return parts.joinToString(" • ").ifBlank { "Defaults" }
}

/**
 * Config screen.
 *
 * Common fields are edited as rows; the full JSON is in the advanced editor
 * below. The punching parameters (birthday / predictive) are numerous and
 * most users never change them, so laying them all out as a form would be
 * worse.
 *
 * Which fields appear follows the strategy. The Go side (tunnel/config.go)
 * restricts ServerConfig structurally: a birthday key, or a
 * predictive.port_scan_range key, makes LoadServerConfig reject the file at
 * load time (not a Validate() failure). So the server role shows no birthday
 * block and no port_scan_range in the predictive block, so the user cannot
 * enter a field the Go side does not allow.
 *
 * Validation goes only through Mobile.validateConfig, which reuses the
 * tunnel package's Validate(), the same rules applied at Start. There is no
 * separate Kotlin validation (two rule sets would drift: the UI says OK and
 * Start fails, or the other way round). The only local check is that
 * required fields are non-empty, which catches no semantic errors but
 * avoids confusing JSON errors when a field was cleared by accident.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ConfigScreen(configStore: ConfigStore, onBack: () -> Unit) {
    // The role is no longer switched on this screen (it moved to the tunnel
    // screen), but it still decides which fields to show. Read configStore.role
    // directly instead of keeping a copy in state: after the role changes on
    // the home screen, this screen must show that role's config, and a local
    // copy could be stale.
    val role = configStore.role
    // unescapeSlashes() is applied to the initial value too: configs saved
    // before it existed still contain the "\/" written by org.json in
    // SharedPreferences, and without cleanup the screen would show
    // "ws:\/\/host" until the first edit went through mutateConfig().
    var raw by remember { mutableStateOf(configStore.currentConfig.unescapeSlashes()) }
    var error by remember { mutableStateOf<String?>(null) }
    var saved by remember { mutableStateOf(false) }
    // The open dialog, or null. A sealed class rather than a dozen booleans
    // guarantees at most one dialog at a time.
    var dialog by remember { mutableStateOf<ConfigDialog?>(null) }

    val interfaces = remember { listUsableInterfaces() }

    // Parse the JSON into readable fields; on failure use an empty object so the user can fix it in the advanced editor.
    val obj = remember(raw) {
        try {
            JSONObject(raw)
        } catch (e: Throwable) {
            JSONObject()
        }
    }

    // Every field update goes through this one function: parse the current
    // raw, modify a JSONObject in the closure, serialize back to raw; on error
    // keep the old value and show the error.
    fun mutateConfig(mutate: (JSONObject) -> Unit) {
        raw = try {
            val obj0 = JSONObject(raw)
            mutate(obj0)
            obj0.toString(2).unescapeSlashes()
        } catch (e: Throwable) {
            error = "Failed to update field: ${e.message}"
            raw
        }
        saved = false
    }

    fun updateField(key: String, value: String) {
        mutateConfig { it.put(key, value) }
    }

    fun updateIntField(key: String, value: String) {
        mutateConfig {
            if (value.isBlank()) it.remove(key) else it.put(key, value.trim().toInt())
        }
    }

    fun updateBoolField(key: String, value: Boolean) {
        mutateConfig { it.put(key, value) }
    }

    /**
     * Updates the signal field and moves the hosts mapping from the old host to
     * the new one (keeping the same IP), so editing the signal host does not
     * leave an orphan hosts entry for the old name.
     *
     * Only migrates if the old host really has a mapping and the new host
     * differs; otherwise (never set, host unchanged) it does nothing.
     */
    fun updateSignalField(newSignal: String) {
        mutateConfig { obj0 ->
            val oldHost = parseSignalHost(obj0.optString("signal"))
            obj0.put("signal", newSignal)
            val newHost = parseSignalHost(newSignal)
            val hosts = obj0.optJSONObject("hosts")
            if (hosts != null && oldHost != null && newHost != null && oldHost != newHost && hosts.has(oldHost)) {
                val ip = hosts.getString(oldHost)
                hosts.remove(oldHost)
                hosts.put(newHost, ip)
                obj0.put("hosts", hosts)
            }
        }
    }

    /** Updates the signal IP input; only maintains the hosts key for the signal host. */
    fun updateSignalIP(signalHost: String?, ip: String) {
        mutateConfig { obj0 -> applySignalIP(obj0, signalHost, ip) }
    }

    /** When the strategy changes, add the matching nested block if missing so the form is not empty after switching. */
    fun updateStrategy(value: String) {
        mutateConfig { obj0 ->
            obj0.put("strategy", value)
            when (value) {
                "birthday" -> if (role == "client" && !obj0.has("birthday")) {
                    obj0.put("birthday", defaultBirthdayJson())
                }
                "predictive" -> if (!obj0.has("predictive")) {
                    obj0.put("predictive", defaultPredictiveJson(role))
                }
            }
        }
    }

    fun updateNestedField(block: String, key: String, value: String, isInt: Boolean = false) {
        mutateConfig { obj0 ->
            val nested = obj0.optJSONObject(block) ?: JSONObject()
            if (isInt) {
                if (value.isBlank()) nested.remove(key) else nested.put(key, value.trim().toInt())
            } else {
                nested.put(key, value)
            }
            obj0.put(block, nested)
        }
    }

    /** Minimal local check before saving: only catches required fields cleared by accident, no semantic validation. */
    fun localCheck(): String? {
        val addrKey = if (role == "server") "forward" else "listen"
        if (obj.optString(addrKey).isBlank()) {
            return if (role == "server") "Forward target is required" else "Local listen address is required"
        }
        // Format check, not semantic validation: obviously malformed addresses are
        // caught here with a specific reason, which is more useful than a generic
        // parse error from Go. Semantic rules (sensible port ranges, strategy
        // constraints) are still checked only on the Go side.
        checkHostPort(obj.optString(addrKey), if (role == "server") "Forward target" else "Local listen")
            ?.let { return it }

        val signal = obj.optString("signal")
        if (signal.isBlank()) return "Signal address is required"
        // Shares checkSignalUri with the as-you-type check so both use the same rules.
        checkSignalUri(signal)?.let { return it }

        if (obj.optString("room").isBlank()) return "Room name is required"

        // The signal IP is optional, but if set it must be a valid IPv4 address.
        val signalHost = parseSignalHost(signal)
        val signalIP = readSignalIP(obj, signalHost)
        if (signalIP.isNotBlank()) {
            checkIPv4(signalIP)?.let { return it }
        }
        // bind_interface is not checked here: it is no longer stored in the config,
        // because interface names change with the network and a stored one goes
        // stale. It is picked at every start instead, and TunnelService.startTunnel
        // injects it into that start's config copy.
        return null
    }

    fun doSave() {
        val localError = localCheck()
        if (localError != null) {
            error = localError
            saved = false
            return
        }
        val message = try {
            Mobile.validateConfig(role, raw)
        } catch (e: Throwable) {
            "Validation failed: ${e.message}"
        }
        if (message.isNullOrEmpty()) {
            // The role is not changed here (the home screen owns it); only the current role's config is written.
            if (role == "server") {
                configStore.serverConfig = raw
            } else {
                configStore.clientConfig = raw
            }
            error = null
            saved = true
            // On success go straight back to the home screen. Only on success: a
            // validation failure must stay on this screen with the reason shown.
            onBack()
        } else {
            error = message
            saved = false
        }
    }

    // Scaffold + TopAppBar rather than two buttons at the top of a Column: this
    // screen is rendered full-screen outside App.kt's Scaffold (showConfig =
    // true), so it does not inherit its insets handling, and plain buttons would
    // sit under the status bar. TopAppBar lays out within the
    // WindowInsets.safeDrawing top area, clear of notches, cutouts and the
    // status bar.
    Scaffold(
        topBar = {
            // The title bar is deliberately 48dp (a standard TopAppBar is 64dp):
            // the config screen has a lot of content, and a smaller fixed header
            // leaves more visible area. Icon buttons in actions keep the bar at the
            // TopAppBar's own height (a filled Button would make it taller).
            TopAppBar(
                title = {
                    Text("Settings", style = MaterialTheme.typography.titleMedium)
                },
                navigationIcon = {
                    IconButton(onClick = onBack) {
                        Icon(
                            imageVector = Icons.AutoMirrored.Filled.ArrowBack,
                            contentDescription = "Back",
                        )
                    }
                },
                actions = {
                    IconButton(onClick = { doSave() }) {
                        Icon(
                            imageVector = Icons.Filled.Check,
                            contentDescription = "Save",
                            tint = MaterialTheme.colorScheme.primary,
                        )
                    }
                },
                expandedHeight = 48.dp,
            )
        },
    ) { innerPadding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                // Only innerPadding: Scaffold already accounts for the status and
                // navigation bars; adding windowInsetsPadding(safeDrawing) would apply
                // the top inset twice and push content down.
                .padding(innerPadding)
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 16.dp, vertical = 8.dp),
        ) {
            // -- Connection: these fields require typing, so they stay text fields;
            // turning addresses/URLs into dialogs would only add a tap.
            SectionHeader("Connection", modifier = Modifier.padding(top = 8.dp))

            // The role decides the first field: local listen for the client, forward target for the server.
            val firstKey = if (role == "server") "forward" else "listen"
            val firstLabel = if (role == "server") "Forward target" else "Local listen"
            // The Uri keyboard avoids CJK input methods; sanitizeAddr also converts
            // full-width colons to half-width and drops non-ASCII/spaces.
            ConfigField(
                firstLabel,
                obj.optString(firstKey),
                keyboardType = KeyboardType.Uri,
                validate = { checkHostPort(it, firstLabel) },
            ) { updateField(firstKey, sanitizeAddr(it)) }

            Spacer(Modifier.height(8.dp))
            // Signal address and signal IP share one display row: they are two halves
            // of one thing (which signal server, and which IP to use to bypass DNS).
            // Two separate fields would take space and look unrelated. Tapping opens
            // one dialog to edit both, and the IP field only appears for a domain
            // name; an IP needs no mapping.
            val signalValue = obj.optString("signal")
            val signalHost = parseSignalHost(signalValue)
            val signalIsIP = signalHost != null && isLikelyIP(signalHost)
            val signalIP = if (signalIsIP) "" else readSignalIP(obj, signalHost)
            ClickableField(
                label = "Signal server",
                value = formatSignalDisplay(signalValue, signalIP),
                placeholder = "ws://example.com:9960/path",
            ) { dialog = ConfigDialog.Signal }

            Spacer(Modifier.height(8.dp))
            // The room name is not here: it pairs the two ends and is used far more
            // often than other settings, so it lives on the tunnel screen, always
            // visible and editable with a tap.

            // -- Punching: all choice-type fields, a quiet group of rows.
            SectionHeader("Punching")
            SettingGroup {
                SettingRow(
                    label = "Strategy",
                    value = obj.optString("strategy"),
                ) { dialog = ConfigDialog.Strategy }
                RowDivider()
                // Max concurrent attempts: row plus dialog; shows the effective default
                // when unset. Default 2, maximum 16 (see Config.MaxConcurrentAttempts in
                // burrow.go); values above 16 make the Go side refuse to start.
                SettingRow(
                    label = "Max concurrent attempts",
                    value = obj.optInt("max_concurrent_attempts", 0)
                        .let { if (it == 0) "2" else it.toString() },
                ) { dialog = ConfigDialog.MaxConcurrent }

                // trials exists only for the client (ClientConfig.Trials in
                // tunnel/config.go). Negative is invalid; 0/empty means the default 100.
                //
                // Like the other rows in this group it is a row plus dialog, not a text
                // field, which would look out of place. The value shows the effective
                // number (100 when unset) rather than a "leave empty for 100" hint: users
                // want to see the current value.
                if (role == "client") {
                    RowDivider()
                    SettingRow(
                        label = "Punch trials",
                        value = obj.optInt("trials", 0).let { if (it == 0) "100" else it.toString() },
                    ) { dialog = ConfigDialog.Trials }
                }
            }

            // Strategy-specific parameters: each block collapses to a one-line summary
            // and is edited in a dialog. birthday is client-only: a birthday key in a
            // server config is rejected by LoadServerConfig at load time (not a
            // validation failure).
            val strategy = obj.optString("strategy")
            if (role == "client" && strategy == "birthday") {
                Spacer(Modifier.height(8.dp))
                SettingGroup {
                    SummaryRow(
                        title = "Birthday parameters",
                        summary = birthdaySummary(obj.optJSONObject("birthday") ?: JSONObject()),
                    ) { dialog = ConfigDialog.Birthday }
                }
            }
            if (strategy == "predictive") {
                Spacer(Modifier.height(8.dp))
                SettingGroup {
                    SummaryRow(
                        title = "Predictive parameters",
                        summary = predictiveSummary(obj.optJSONObject("predictive") ?: JSONObject()),
                    ) { dialog = ConfigDialog.Predictive }
                }
            }

            // -- Server options: two booleans as standard switch rows.
            if (role == "server") {
                SectionHeader("Server options")
                SettingGroup {
                    // Booleans follow the same one-way pattern as other fields (read
                    // obj, write back raw) with no extra local mutableStateOf: obj is
                    // already derived via remember(raw), and a second boolean state
                    // could get out of sync.
                    SwitchRow(
                        label = "Stable side",
                        checked = obj.optBoolean("stable_side"),
                        description = "Required on the server for the birthday strategy",
                    ) { updateBoolField("stable_side", it) }
                    RowDivider()
                    SwitchRow(
                        label = "Skip NAT self-check",
                        checked = obj.optBoolean("skip_nat_check"),
                        description = "Bypass a false self-check result. Normally off.",
                    ) { updateBoolField("skip_nat_check", it) }
                }
            }

            // -- Other
            SectionHeader("Other")
            SettingGroup {
                SettingRow(
                    label = "Log level",
                    // When empty, the Go side Validate() fills in info.
                    value = obj.optString("log_level").ifBlank { "info (default)" },
                ) { dialog = ConfigDialog.LogLevel }
            }

            // mode is not in the common section; it can be edited in the advanced JSON editor.

            // The full JSON editor is a single entry row: an inline 320dp editor would
            // take most of the screen, and most users never edit JSON by hand. It opens
            // in a full-screen dialog with enough room.
            SectionHeader("Advanced")
            SettingGroup {
                SettingRow(
                    label = "Full config",
                    value = "Tap to edit JSON",
                ) { dialog = ConfigDialog.RawJson }
            }
            Spacer(Modifier.height(8.dp))
            Text(
                "Less common fields such as mode and hosts can be edited in the full config. Validated on save.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )

            error?.let {
                Spacer(Modifier.height(8.dp))
                Text(it, color = MaterialTheme.colorScheme.error)
            }
            Spacer(Modifier.height(24.dp))
        }

        // -- Dialogs. At most one is open at a time (dialog is a single sealed value).
        when (dialog) {
            ConfigDialog.Signal -> SignalDialog(
                initialSignal = obj.optString("signal"),
                initialIP = parseSignalHost(obj.optString("signal"))
                    ?.takeIf { !isLikelyIP(it) }
                    ?.let { readSignalIP(obj, it) }
                    ?: "",
                onDismiss = { dialog = null },
                onConfirm = { newSignal, newIP ->
                    // Write the address first (updateSignalField also migrates the hosts
                    // entry from the old host), then the IP under the new host; the other
                    // order would write the IP under the old host's key.
                    updateSignalField(newSignal)
                    updateSignalIP(parseSignalHost(newSignal), newIP)
                },
            )

            ConfigDialog.Strategy -> SingleChoiceDialog(
                title = "Strategy",
                options = listOf("direct", "predictive", "birthday"),
                selected = obj.optString("strategy"),
                descriptions = mapOf(
                    "direct" to "Direct, no port prediction",
                    "predictive" to "Predict the peer's port and scan around it",
                    "birthday" to "Birthday collision over many rounds",
                ),
                onDismiss = { dialog = null },
                onSelect = { updateStrategy(it) },
            )

            // These four are the only valid log levels (see Validate in
            // tunnel/config.go). There is no "empty" option: Go fills an empty value
            // with info, so listing an "equivalent to info" choice would only
            // confuse. When unset, the row shows "info (default)".
            ConfigDialog.LogLevel -> SingleChoiceDialog(
                title = "Log level",
                options = listOf("debug", "info", "warn", "error"),
                selected = obj.optString("log_level").ifBlank { "info" },
                descriptions = mapOf(
                    "debug" to "Most verbose, for troubleshooting",
                    "info" to "Normal operation (default)",
                    "warn" to "Warnings and errors only",
                    "error" to "Errors only",
                ),
                onDismiss = { dialog = null },
                onSelect = { updateField("log_level", it) },
            )

            ConfigDialog.MaxConcurrent -> TextEditDialog(
                title = "Max concurrent attempts",
                initial = obj.optInt("max_concurrent_attempts", 0)
                    .let { if (it == 0) "" else it.toString() },
                hint = "Empty means the default, 2. Maximum 16 (higher values refuse to start). Limits in-flight punch attempts per Dialer/Listener.",
                keyboardType = KeyboardType.Number,
                onDismiss = { dialog = null },
                onConfirm = { updateIntField("max_concurrent_attempts", sanitizeDigits(it)) },
            )

            ConfigDialog.Trials -> TextEditDialog(
                title = "Punch trials",
                initial = obj.optInt("trials", 0).let { if (it == 0) "" else it.toString() },
                hint = "Empty means the default, 100. Birthday punching relies on many rounds: more trials raise the hit rate but take longer.",
                keyboardType = KeyboardType.Number,
                onDismiss = { dialog = null },
                onConfirm = { updateIntField("trials", sanitizeDigits(it)) },
            )

            // The whole birthday block is edited in one dialog (a pile of text fields would be too noisy).
            ConfigDialog.Birthday -> {
                val b = obj.optJSONObject("birthday") ?: JSONObject()
                MultiFieldDialog(
                    title = "Birthday parameters",
                    fields = listOf(
                        DialogField(
                            "selection", "Selection", b.optString("selection"),
                            options = listOf("ack", "fanout"),
                        ),
                        DialogField("candidates", "Candidates", b.intOrBlank("candidates"), isInt = true),
                        DialogField("min_port", "Min port", b.intOrBlank("min_port"), isInt = true),
                        DialogField("max_port", "Max port", b.intOrBlank("max_port"), isInt = true),
                        DialogField(
                            "max_success_candidates", "Max successful candidates",
                            b.intOrBlank("max_success_candidates"), isInt = true,
                        ),
                        DialogField("warmup", "Warmup (e.g. 250ms)", b.optString("warmup")),
                        DialogField("attempt_timeout", "Attempt timeout (e.g. 5s)", b.optString("attempt_timeout")),
                        DialogField("success_grace", "Success grace (e.g. 250ms)", b.optString("success_grace")),
                    ),
                    onDismiss = { dialog = null },
                    onConfirm = { values ->
                        val ints = setOf(
                            "candidates", "min_port", "max_port", "max_success_candidates",
                        )
                        // Write all fields back in one mutateConfig call rather than per
                        // field: each call re-parses and re-serializes the whole JSON (8
                        // rounds for 8 fields), and a parse failure midway would leave a
                        // half-applied update.
                        mutateConfig { obj0 ->
                            val nested = obj0.optJSONObject("birthday") ?: JSONObject()
                            values.forEach { (k, v) -> nested.putField(k, v, k in ints) }
                            obj0.put("birthday", nested)
                        }
                    },
                )
            }

            ConfigDialog.Predictive -> {
                val p = obj.optJSONObject("predictive") ?: JSONObject()
                MultiFieldDialog(
                    title = "Predictive parameters",
                    fields = buildList {
                        // port_scan_range is shown only for the client: a server config
                        // containing this key is rejected by LoadServerConfig.
                        if (role == "client") {
                            add(
                                DialogField(
                                    "port_scan_range", "Port scan range",
                                    p.intOrBlank("port_scan_range"), isInt = true,
                                ),
                            )
                        }
                        add(DialogField("concurrency", "Concurrency", p.intOrBlank("concurrency"), isInt = true))
                        add(
                            DialogField(
                                "attempt_timeout", "Attempt timeout (e.g. 5s)",
                                p.optString("attempt_timeout"),
                            ),
                        )
                    },
                    onDismiss = { dialog = null },
                    onConfirm = { values ->
                        val ints = setOf("port_scan_range", "concurrency")
                        mutateConfig { obj0 ->
                            val nested = obj0.optJSONObject("predictive") ?: JSONObject()
                            values.forEach { (k, v) -> nested.putField(k, v, k in ints) }
                            obj0.put("predictive", nested)
                        }
                    },
                )
            }

            // Full JSON: full-screen dialog. OK writes back raw, and obj is derived
            // via remember(raw), so every row above updates automatically.
            ConfigDialog.RawJson -> RawJsonDialog(
                initial = raw,
                onDismiss = { dialog = null },
                onConfirm = {
                    raw = it
                    saved = false
                },
            )

            null -> Unit
        }
    }
}

/**
 * Full-screen editor for the complete JSON.
 *
 * Dialog with usePlatformDefaultWidth = false rather than AlertDialog: the
 * latter is too narrow and short for formatted JSON, so monospace lines
 * keep wrapping and the indentation becomes unreadable.
 *
 * Same transaction semantics as TextEditDialog: the text being edited lives
 * in the dialog's state, Cancel or tapping outside leaves raw unchanged, and
 * only OK writes it back.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun RawJsonDialog(
    initial: String,
    onDismiss: () -> Unit,
    onConfirm: (String) -> Unit,
) {
    var text by remember { mutableStateOf(initial) }
    Dialog(
        onDismissRequest = onDismiss,
        properties = DialogProperties(usePlatformDefaultWidth = false),
    ) {
        Surface(modifier = Modifier.fillMaxSize()) {
            Scaffold(
                topBar = {
                    TopAppBar(
                        title = { Text("Full config") },
                        navigationIcon = {
                            IconButton(onClick = onDismiss) {
                                Icon(
                                    imageVector = Icons.AutoMirrored.Filled.ArrowBack,
                                    contentDescription = "Cancel",
                                )
                            }
                        },
                        actions = {
                            Button(onClick = { onConfirm(text); onDismiss() }) { Text("OK") }
                            Spacer(Modifier.width(8.dp))
                        },
                    )
                },
            ) { padding ->
                TextField(
                    colors = appTextFieldColors(),
                    value = text,
                    onValueChange = { text = it },
                    modifier = Modifier
                        .fillMaxSize()
                        .padding(padding)
                        .windowInsetsPadding(WindowInsets.safeDrawing)
                        .padding(horizontal = 12.dp),
                    textStyle = MaterialTheme.typography.bodySmall.copy(
                        fontFamily = FontFamily.Monospace,
                    ),
                )
            }
        }
    }
}

/** Reads an int field; 0 (unset) reads as an empty string so the input shows its placeholder instead of "0". */
private fun JSONObject.intOrBlank(key: String): String =
    optInt(key, 0).let { if (it == 0) "" else it.toString() }

/** Writes a field by type; an empty string means "unset" and removes the key instead of writing 0/"". */
private fun JSONObject.putField(key: String, value: String, isInt: Boolean) {
    val v = value.trim()
    when {
        v.isBlank() -> remove(key)
        isInt -> v.toIntOrNull()?.let { put(key, it) } ?: remove(key)
        else -> put(key, v)
    }
}

/**
 * A read-only field that looks exactly like ConfigField and opens a dialog
 * on tap.
 *
 * Used for values edited together in a dialog (signal address + signal
 * IP). It must look identical to a regular field (same label, underline and
 * height), or it would stand out among the text fields.
 *
 * readOnly rather than enabled = false: the latter greys out the text and
 * looks unavailable, while this field is editable through the dialog. A
 * clickable Box takes over taps, since the TextField's own click area does
 * not fire onClick when readOnly.
 */
@Composable
private fun ClickableField(
    label: String,
    value: String,
    placeholder: String = "",
    modifier: Modifier = Modifier,
    onClick: () -> Unit,
) {
    Box(modifier = modifier.fillMaxWidth()) {
        TextField(
            colors = appTextFieldColors(),
            value = value,
            onValueChange = {},
            readOnly = true,
            label = { Text(label) },
            placeholder = if (placeholder.isNotEmpty()) {
                { Text(placeholder) }
            } else {
                null
            },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        // A transparent overlay over the whole field takes over taps. It sits
        // above the TextField, so tapping anywhere (including on the text) opens
        // the dialog instead of placing the cursor.
        Box(
            modifier = Modifier
                .matchParentSize()
                .clickable(onClick = onClick),
        )
    }
}

@Composable
private fun ConfigField(
    label: String,
    value: String,
    keyboardType: KeyboardType = KeyboardType.Text,
    modifier: Modifier = Modifier,
    validate: (String) -> String? = { null },
    onChange: (String) -> Unit,
) {
    // Filled TextField rather than OutlinedTextField: the config screen has a
    // dozen fields, and outlining each would be a wall of visual noise. Colors
    // come from appTextFieldColors(): transparent container with a thin bottom
    // line, lighter than the default grey block.
    //
    // Validate as the user types: errors show in red under the field right
    // away, not at the bottom after Save when the user has already moved on.
    // Empty values are not errors (unfinished, not wrong); localCheck() catches
    // them on save.
    val errorText = if (value.isBlank()) null else validate(value)
    Column(modifier = modifier.fillMaxWidth()) {
        TextField(
            colors = appTextFieldColors(),
            value = value,
            onValueChange = onChange,
            label = { Text(label) },
            singleLine = true,
            isError = errorText != null,
            keyboardOptions = KeyboardOptions(keyboardType = keyboardType),
            modifier = Modifier.fillMaxWidth(),
        )
        if (errorText != null) {
            Text(
                errorText,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.error,
                modifier = Modifier.padding(start = 16.dp, top = 4.dp),
            )
        }
    }
}

/**
 * Default parameter block added when switching to the birthday strategy.
 *
 * Parsed from ConfigStore.DEFAULT_CLIENT instead of repeating the values
 * here, so there is a single source of truth that cannot drift.
 */
private fun defaultBirthdayJson(): JSONObject =
    JSONObject(ConfigStore.DEFAULT_CLIENT).optJSONObject("birthday") ?: JSONObject()

/**
 * Default parameter block added when switching to the predictive strategy;
 * port_scan_range is omitted for the server role (a Go-side structural
 * restriction). Values come from ConfigStore.DEFAULT_PREDICTIVE, the single
 * source of truth.
 */
private fun defaultPredictiveJson(role: String): JSONObject {
    val obj = JSONObject(ConfigStore.DEFAULT_PREDICTIVE)
    if (role == "server") {
        obj.remove("port_scan_range")
    }
    return obj
}
