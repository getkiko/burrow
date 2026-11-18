package com.getkiko.burrow

import java.net.NetworkInterface

/**
 * Lists the usable physical network interfaces on this device.
 *
 * Only interfaces that are up, not loopback and have an IPv4 address are
 * returned. Hole punching must bind to a routable physical interface; binding
 * the wrong one sends the SYN out the wrong interface and punching always
 * fails. It is also the setting people most often get wrong on the CLI (eth0
 * on Linux, en0 on macOS), which is why the UI offers a dropdown.
 *
 * The order is the priority: Wi-Fi > mobile data > others. The first entry is
 * the sensible default, so callers can use first() as the auto-selected
 * interface; on almost every device there is only one right answer.
 */
fun listUsableInterfaces(): List<String> =
    try {
        NetworkInterface.getNetworkInterfaces()
            .asSequence()
            .filter { iface ->
                iface.isUp &&
                    !iface.isLoopback &&
                    // Skip virtual tunnel interfaces: tun/utun are created by VPNs, and
                    // binding to one loops the punching traffic back through the proxy, which
                    // never yields the real public endpoint and can never connect. They are
                    // also up with an IPv4 address, so the other checks do not filter them.
                    !isVirtualInterface(iface.name) &&
                    iface.inetAddresses.asSequence().any { addr ->
                        !addr.isLoopbackAddress && addr.address.size == 4
                    }
            }
            .map { it.name }
            .sortedBy { interfacePriority(it) }
            .toList()
    } catch (e: Throwable) {
        emptyList()
    }

/** tun/utun/tap are virtual interfaces created by VPNs and cannot carry punching traffic. */
private fun isVirtualInterface(name: String): Boolean {
    val lower = name.lowercase()
    return lower.startsWith("tun") || lower.startsWith("utun") || lower.startsWith("tap")
}

/**
 * Interface priority; lower is preferred.
 *
 * Wi-Fi comes first: tunnels on phones almost always run on Wi-Fi, and mobile
 * data usually sits behind a stricter carrier-grade NAT (mostly symmetric),
 * where punching succeeds far less often.
 *
 * rmnet/ccmni are the mobile data interface names used by different chipsets
 * (Qualcomm rmnet_dataN, MediaTek ccmniN). They rank after Wi-Fi but before
 * everything else: they are at least a real uplink, while "others" may include
 * VPN interfaces, USB tethering and the like.
 */
private fun interfacePriority(name: String): Int = when {
    name.startsWith("wlan") -> 0
    name.startsWith("eth") -> 1
    name.startsWith("rmnet") || name.startsWith("ccmni") -> 2
    else -> 3
}
