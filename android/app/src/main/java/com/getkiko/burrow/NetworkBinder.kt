package com.getkiko.burrow

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Build

/**
 * Binds this process's network traffic to a physical network, bypassing any
 * VPN/tun.
 *
 * ## Why
 *
 * The typical case is running alongside another app's VpnService (a
 * system-wide VPN): the default route is taken over by its tun interface. If
 * the signal connection and punching packets used the default route they
 * would be captured by that VPN: at best they take a detour and time out, at
 * worst the signal server sees the VPN's exit IP and the punching candidates
 * are wrong from the start.
 *
 * ## Why not SO_BINDTODEVICE / SO_BINDTOIFINDEX on the socket
 *
 * Neither is reliable in an Android app process:
 *
 * - `SO_BINDTODEVICE` requires CAP_NET_RAW, which apps do not have, so it
 *   always fails with EPERM.
 * - `SO_BINDTOIFINDEX` needs no privilege but requires Linux 5.7+. Android 11
 *   ships 5.4 and returns ENOPROTOOPT, which rules out many devices in use.
 *
 * `bindProcessToNetwork` is the official app API: it needs no permission,
 * works on API 23+, and is process-wide. Every socket created afterwards
 * (including those created by Go through gomobile) uses that network, with no
 * per-socket handling and no need to pass a Network object into Go.
 *
 * ## Caveat
 *
 * The binding affects the whole process, so it is only held while the tunnel
 * runs and must be released on stop; otherwise the app's other requests (and
 * anything after a network switch) stay pinned to the old network.
 */
class NetworkBinder(
    context: Context,
    /**
     * Diagnostic log sink, written straight to the log screen.
     *
     * A bind failure aborts the start, so the user must be able to see why it
     * failed. Every branch that can make binding fail logs enough to pinpoint
     * it (number of networks, each network's capabilities, exception details).
     */
    private val log: (String, String) -> Unit = { _, _ -> },
) {

    private val connectivity =
        context.applicationContext.getSystemService(Context.CONNECTIVITY_SERVICE)
            as? ConnectivityManager

    private var callback: ConnectivityManager.NetworkCallback? = null

    private var watchCallback: ConnectivityManager.NetworkCallback? = null

    /**
     * Watches for the interface going away and calls back so the caller can
     * stop the tunnel.
     *
     * Interface names change with the network (mobile data <-> Wi-Fi, airplane
     * mode, hotspot reconnect), while the Go punching sockets stay bound to the
     * interface chosen at start. Once it is gone the sockets silently drop
     * everything while the status still says connected. Better to stop and
     * explain, so the user can pick an interface again.
     *
     * @param interfaceName the interface the tunnel is bound to
     * @param onLost called (on a system callback thread) when it is no longer usable
     */
    fun watchInterface(interfaceName: String, onLost: () -> Unit) {
        val manager = connectivity ?: return
        stopWatching()
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onLost(network: Network) {
                // onLost reports the network that went away, but what matters is
                // whether the interface chosen at start still exists. Checking the
                // current interface list is more direct and also covers an interface
                // that is still there under a different name.
                if (!listUsableInterfaces().contains(interfaceName)) {
                    onLost()
                }
            }
        }
        watchCallback = cb
        try {
            manager.registerDefaultNetworkCallback(cb)
        } catch (e: Throwable) {
            watchCallback = null
        }
    }

    fun stopWatching() {
        val manager = connectivity ?: return
        watchCallback?.let {
            try {
                manager.unregisterNetworkCallback(it)
            } catch (e: Throwable) {
                // Throws when not registered; ignore.
            }
        }
        watchCallback = null
    }

    /**
     * Requests a real physical network and binds the process to it,
     * synchronously.
     *
     * First tries getAllNetworks() for an already connected non-VPN network,
     * then falls back to requestNetwork(). The request explicitly requires
     * NOT_VPN; without it the system may well return the VPN's tun network as
     * the available network, which bypasses nothing.
     *
     * It must wait synchronously: requestNetwork's onAvailable is an
     * asynchronous callback, and the caller has the Go side open sockets right
     * after this returns. Without waiting, the binding often misses the first
     * connection attempt and the user still gets "network is unreachable". The
     * wait is bounded by timeoutMillis.
     *
     * @return a description of the bound network (e.g. "Mobile data"), or null
     *   on failure.
     */
    fun bind(timeoutMillis: Long = 3000): String? {
        val manager = connectivity
        if (manager == null) {
            log("error", "Bind failed: ConnectivityManager unavailable")
            return null
        }
        unbind()

        // Try getAllNetworks() first: it only needs ACCESS_NETWORK_STATE (which the
        // app always has) and is synchronous, so there is no callback delay and no
        // failure from a missing CHANGE_NETWORK_STATE. requestNetwork() needs that
        // permission; it is declared in the manifest, but the permission-free path
        // is the more robust primary.
        pickPhysicalNetwork(manager)?.let { network ->
            if (bindTo(manager, network)) {
                return describe(manager, network)
            }
            log("warn", "Network selected but bindProcessToNetwork failed; trying requestNetwork")
        }

        // Fallback: when no non-VPN network is connected right now (e.g. right after
        // toggling airplane mode), requestNetwork waits for one to become available,
        // which the getAllNetworks snapshot cannot do.
        log("info", "No usable network found; waiting up to ${timeoutMillis}ms via requestNetwork")
        return awaitNetwork(manager, timeoutMillis)
    }

    /**
     * Picks a real physical network from the currently connected ones.
     *
     * Excluding VPNs is the point: otherwise, with a VPN's tun up, the virtual
     * interface may well be picked, bypassing nothing. Requires both NOT_VPN and
     * INTERNET, and prefers networks the system has VALIDATED.
     */
    private fun pickPhysicalNetwork(manager: ConnectivityManager): Network? {
        val networks = try {
            @Suppress("DEPRECATION")
            manager.allNetworks
        } catch (e: Throwable) {
            // Log rather than silently return null, so "enumeration threw" can be
            // told apart from "networks found but all filtered out".
            log("error", "Failed to list networks: ${e.javaClass.simpleName}: ${e.message}")
            return null
        }
        log("info", "Found ${networks.size} network(s)")
        var fallback: Network? = null
        for (network in networks) {
            val caps = try {
                manager.getNetworkCapabilities(network)
            } catch (e: Throwable) {
                log("warn", "  $network: failed to get capabilities: ${e.message}")
                null
            }
            if (caps == null) {
                // getNetworkCapabilities returns null (not an exception) for a network that has gone away.
                log("warn", "  $network: no capabilities, skipped")
                continue
            }
            // Log every network whether or not it is picked: when none is picked,
            // what matters is which capability each skipped network lacked.
            val summary = describeCaps(caps)
            if (!caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)) {
                log("warn", "  $network: $summary -> skipped (no INTERNET)")
                continue
            }
            if (!caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)) {
                log("warn", "  $network: $summary -> skipped (VPN/tun)")
                continue
            }
            // VALIDATED means the system verified internet access; prefer it, otherwise keep as a candidate.
            if (caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)) {
                log("info", "  $network: $summary -> selected")
                return network
            }
            if (fallback == null) {
                log("info", "  $network: $summary -> kept as candidate (not VALIDATED)")
                fallback = network
            } else {
                log("info", "  $network: $summary -> skipped (candidate already chosen)")
            }
        }
        if (fallback == null) {
            log("warn", "No network has both INTERNET and NOT_VPN")
        }
        return fallback
    }

    /** Flattens capabilities and transports into one line for diagnostic logs. */
    private fun describeCaps(caps: NetworkCapabilities): String {
        val transports = buildList {
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) add("WIFI")
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) add("CELLULAR")
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) add("ETHERNET")
            if (caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) add("VPN")
        }.joinToString("+").ifEmpty { "no known transport" }
        val flags = buildList {
            if (caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)) add("INTERNET")
            if (caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)) add("NOT_VPN")
            if (caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)) add("VALIDATED")
        }.joinToString(",").ifEmpty { "none" }
        return "$transports [$flags]"
    }

    /** requestNetwork path: needs CHANGE_NETWORK_STATE and throws without it. */
    private fun awaitNetwork(manager: ConnectivityManager, timeoutMillis: Long): String? {
        val request = NetworkRequest.Builder()
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
            .build()

        val latch = java.util.concurrent.CountDownLatch(1)
        val bound = java.util.concurrent.atomic.AtomicReference<Network?>(null)

        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                bound.compareAndSet(null, network)
                latch.countDown()
            }
        }
        callback = cb
        try {
            manager.requestNetwork(request, cb)
        } catch (e: Throwable) {
            // A missing CHANGE_NETWORK_STATE throws SecurityException; some custom
            // ROMs may throw something else. Log it so "permission denied" can be
            // told apart from "timed out", which call for different fixes.
            log("error", "requestNetwork failed: ${e.javaClass.simpleName}: ${e.message}")
            callback = null
            return null
        }

        try {
            latch.await(timeoutMillis, java.util.concurrent.TimeUnit.MILLISECONDS)
        } catch (e: InterruptedException) {
            Thread.currentThread().interrupt()
        }

        val network = bound.get()
        if (network == null) {
            log("error", "requestNetwork timed out after ${timeoutMillis}ms; no non-VPN network available")
            return null
        }
        if (bindTo(manager, network)) {
            return describe(manager, network)
        }
        log("error", "requestNetwork returned a network but bindProcessToNetwork failed")
        return null
    }

    private fun bindTo(manager: ConnectivityManager, network: Network): Boolean = try {
        val ok = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            manager.bindProcessToNetwork(network)
        } else {
            @Suppress("DEPRECATION")
            ConnectivityManager.setProcessDefaultNetwork(network)
        }
        // Returning false and throwing are different failures; log them separately.
        if (!ok) log("warn", "bindProcessToNetwork($network) returned false")
        ok
    } catch (e: Throwable) {
        log("error", "bindProcessToNetwork threw: ${e.javaClass.simpleName}: ${e.message}")
        false
    }

    /**
     * Unbinds and releases the network request.
     *
     * Must be called when the tunnel stops: binding is process-wide, so without
     * it every later request from the app stays pinned to this network and does
     * not follow a switch to Wi-Fi.
     */
    fun unbind() {
        val manager = connectivity ?: return
        callback?.let {
            try {
                manager.unregisterNetworkCallback(it)
            } catch (e: Throwable) {
                // unregisterNetworkCallback throws when not registered; ignore.
            }
        }
        callback = null
        try {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
                manager.bindProcessToNetwork(null)
            } else {
                @Suppress("DEPRECATION")
                ConnectivityManager.setProcessDefaultNetwork(null)
            }
        } catch (e: Throwable) {
            // Ignored: nothing can be done if unbinding fails, and it does not affect stopping.
        }
    }

    /** Network description for logs, e.g. "Mobile data" / "Wi-Fi". */
    private fun describe(manager: ConnectivityManager, network: Network): String {
        val caps = manager.getNetworkCapabilities(network) ?: return "unknown network"
        return when {
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "Wi-Fi"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "Mobile data"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "Ethernet"
            else -> "other network"
        }
    }
}
