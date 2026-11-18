package burrow

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

// This file collects the UDP NAT profile.
//
// Deliberate difference from the TCP version (profile.go): TCP uses a new
// local address for every sample, while UDP reuses one socket, because what
// is measured is whether that socket keeps the same mapping towards
// different STUN targets. Classification relies only on comparing mappings
// across targets; no separate NAT type detection is performed.
//
// Socket-phase invariant: collectUDPProfileSample calls queryUDPMappedAddr
// sequentially on the given socket (at most one STUN transaction in flight),
// and a socket must finish STUN before any candidate packet is sent on it.
// The caller must not send candidate packets on the socket until this
// function returns. The state machine in packet_session.go enforces this
// order: profile collection and STUN mapping first, then armPredictive or
// armDirect for candidate packets.

// collectUDPProfileSample queries each STUN server in servers, in order,
// from the same conn and fills one profileSample. Calling
// queryUDPMappedAddr once per target is what gives "one socket, several
// STUN targets, queried sequentially".
func collectUDPProfileSample(ctx context.Context, conn net.PacketConn, servers []string, timeout time.Duration) (profileSample, error) {
	sample := profileSample{}
	for _, server := range servers {
		if err := ctx.Err(); err != nil {
			return profileSample{}, err
		}
		addr, err := queryUDPMappedAddr(conn, server, timeout)
		if err != nil {
			return profileSample{}, err
		}
		sample.publicIPs = append(sample.publicIPs, addr.IP.String())
		sample.ports = append(sample.ports, addr.Port)
	}
	return sample, nil
}

// collectUDPProfile samples with collectUDPProfileSample until at least
// initialSamples samples yield a definite class, or maxSamples is reached.
// Classification thresholds are shared with the TCP path through
// classifyNATProfile (profile.go).
//
// The same conn is used throughout and must not carry candidate packets
// while sampling; the caller (packet_session.go) hands the conn to the
// candidate phase only after this function returns.
func collectUDPProfile(ctx context.Context, conn net.PacketConn, servers []string, timeout time.Duration, initialSamples, maxSamples int, scanRange int) (signalproto.NATProfile, error) {
	crossIP := hasMultipleProbeIPs(servers)
	samples := make([]profileSample, 0, maxSamples)
	for len(samples) < maxSamples {
		block := initialSamples
		if len(samples) > 0 {
			block = 1
		}
		if remaining := maxSamples - len(samples); block > remaining {
			block = remaining
		}
		for range block {
			sample, err := collectUDPProfileSample(ctx, conn, servers, timeout)
			if err != nil {
				return signalproto.NATProfile{}, err
			}
			samples = append(samples, sample)
		}
		profile := classifyNATProfile(samples, scanRange, crossIP)
		if profile.Class != signalproto.NATUnknown {
			return profile, nil
		}
	}
	return classifyNATProfile(samples, scanRange, crossIP), nil
}

// udpMappedAddr queries the mapping of conn against a single STUN server.
// The direct strategy uses it because it needs only the socket's current
// public mapping, not a full profile classification.
func udpMappedAddr(_ context.Context, conn net.PacketConn, server string, timeout time.Duration) (netip.AddrPort, error) {
	addr, err := queryUDPMappedAddr(conn, server, timeout)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return udpAddrToAddrPort(addr)
}

// udpAddrToAddrPort converts a *net.UDPAddr to a netip.AddrPort for exported
// types such as PacketPath.
func udpAddrToAddrPort(addr *net.UDPAddr) (netip.AddrPort, error) {
	ip, ok := netip.AddrFromSlice(addr.IP.To4())
	if !ok {
		ip, ok = netip.AddrFromSlice(addr.IP.To16())
		if !ok {
			return netip.AddrPort{}, fmt.Errorf("burrow: invalid UDP address %v", addr)
		}
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(addr.Port)), nil
}

// splitUDPHostPort splits "ip:port" and parses the port as an integer, for
// expanding the predictive port window.
func splitUDPHostPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}
