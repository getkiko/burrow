package com.getkiko.burrow

import android.content.Context
import android.content.SharedPreferences

/**
 * Config storage.
 *
 * The config is stored as a single JSON string, matching the binding layer's
 * interface, so new fields need no changes here. The role is stored
 * separately because it decides which Start method to call.
 */
class ConfigStore(context: Context) {
    private val prefs: SharedPreferences =
        context.getSharedPreferences("burrow", Context.MODE_PRIVATE)

    var role: String
        get() = prefs.getString(KEY_ROLE, "client") ?: "client"
        set(value) = prefs.edit().putString(KEY_ROLE, value).apply()

    /**
     * This device's default room name, generated randomly on first use and
     * persisted.
     *
     * The room name is what pairs the two ends. A fixed "my-room" would put every
     * user in the same room by default, where whoever connects first pairs up and
     * everyone interferes. A random name makes the default unique.
     *
     * It must be stored, not recomputed: DEFAULT_CLIENT/DEFAULT_SERVER are the
     * getString fallbacks and are evaluated on every read, so a computed name
     * would change on every read and the two ends could never match.
     */
    val defaultRoom: String
        get() = prefs.getString(KEY_ROOM, null) ?: generateRoom().also {
            prefs.edit().putString(KEY_ROOM, it).apply()
        }

    var clientConfig: String
        get() = prefs.getString(KEY_CLIENT, null) ?: defaultClient()
        set(value) = prefs.edit().putString(KEY_CLIENT, value).apply()

    var serverConfig: String
        get() = prefs.getString(KEY_SERVER, null) ?: defaultServer()
        set(value) = prefs.edit().putString(KEY_SERVER, value).apply()

    /**
     * Replaces the placeholders in the default template with this device's
     * values (currently the random room name).
     */
    private fun defaultClient(): String = fillDefaults(DEFAULT_CLIENT)

    private fun defaultServer(): String = fillDefaults(DEFAULT_SERVER)

    private fun fillDefaults(template: String): String = template
        .replace(ROOM_PLACEHOLDER, defaultRoom)

    /** The config for the current role. */
    val currentConfig: String
        get() = if (role == "server") serverConfig else clientConfig

    /**
     * The last value of each input on the debug screen, stored by key.
     *
     * Diagnostic parameters (signal server address, port list, ...) are reused
     * throughout a troubleshooting session; they should not be blank every time
     * the user comes back. Callers supply the default; a stored value wins.
     */
    fun diagInput(key: String, fallback: String): String =
        prefs.getString("$KEY_DIAG_PREFIX$key", null) ?: fallback

    fun setDiagInput(key: String, value: String) {
        prefs.edit().putString("$KEY_DIAG_PREFIX$key", value).apply()
    }

    companion object {
        private const val KEY_ROLE = "role"
        private const val KEY_CLIENT = "client_config"
        private const val KEY_SERVER = "server_config"
        private const val KEY_ROOM = "default_room"
        private const val KEY_DIAG_PREFIX = "diag_"

        /** Room placeholder in the default templates, replaced with the random room on read. */
        private const val ROOM_PLACEHOLDER = "__ROOM__"

        /**
         * Generates a room name of the form "AA123456": 2 uppercase letters and 6
         * digits.
         *
         * The leading letters make it read as an identifier rather than a port or
         * serial number; the fixed length of 8 makes it easy to read out or copy
         * by hand to the other side.
         */
        fun generateRoom(): String {
            val letters = ('A'..'Z')
            val prefix = (1..2).map { letters.random() }.joinToString("")
            val digits = (1..6).map { ('0'..'9').random() }.joinToString("")
            return prefix + digits
        }

        // The defaults list every field so they are easy to edit. bind_interface
        // is left empty: interface names change with the network (Wi-Fi <-> mobile
        // data, airplane mode or a hotspot reconnect can remove an interface
        // entirely), so a stored name goes stale and the tunnel silently fails to
        // connect. Instead the interface is chosen at each start, so it always
        // exists.
        val DEFAULT_CLIENT = """
{
  "listen": "127.0.0.1:10809",
  "signal": "ws://example.com:9960/path",
  "room": "__ROOM__",
  "bind_interface": "",
  "strategy": "birthday",
  "mode": "header",
  "trials": 100,
  "birthday": {
    "selection": "fanout",
    "candidates": 256,
    "min_port": 1024,
    "max_port": 65535,
    "warmup": "250ms",
    "attempt_timeout": "5s",
    "success_grace": "250ms",
    "max_success_candidates": 8
  },
  "log_level": "info"
}
""".trim()

        val DEFAULT_SERVER = """
{
  "forward": "127.0.0.1:8080",
  "signal": "ws://example.com:9960/path",
  "room": "__ROOM__",
  "bind_interface": "",
  "strategy": "birthday",
  "mode": "header",
  "stable_side": true,
  "log_level": "info"
}
""".trim()

        // Default parameter block for strategy=predictive. The default strategy in
        // DEFAULT_CLIENT/DEFAULT_SERVER is birthday, so it is not included there,
        // but the config screen needs defaults when the user switches to
        // predictive. Defined here as the single source of truth so the config
        // screen does not keep its own copy that could drift.
        val DEFAULT_PREDICTIVE = """
{
  "port_scan_range": 512,
  "concurrency": 16,
  "attempt_timeout": "5s"
}
""".trim()
    }
}
