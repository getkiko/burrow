package burrow

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

type profileSample struct {
	publicIPs []string
	ports     []int
}

type cachedNATProfile struct {
	value         signalproto.NATProfile
	expiresAt     time.Time
	interfaceName string
	targets       string
}

func (d *Dialer) natProfile(ctx context.Context) (signalproto.NATProfile, error) {
	interfaceName := d.cfg.interfaceName()
	targets := strings.Join(d.cfg.NATProbe.Targets, "\x00")
	now := time.Now()
	d.profileMu.Lock()
	defer d.profileMu.Unlock()
	if cached := d.profile; cached != nil && now.Before(cached.expiresAt) && cached.interfaceName == interfaceName && cached.targets == targets {
		return cached.value, nil
	}
	probeBudget := d.cfg.NATProbe.Timeout * time.Duration(d.cfg.NATProbe.MaxSamples)
	probeCtx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	profile, err := collectNATProfile(probeCtx, d.cfg)
	if err != nil {
		return signalproto.NATProfile{}, err
	}
	debugLogf(d.cfg.DebugLog, "NAT profile: class=%s public_ip=%s samples=%d prediction=%d/%d cross_ip=%v",
		profile.Class, profile.PublicIP, profile.Samples, profile.PredictionHits, profile.PredictionTrials, profile.VerifiedCrossIP)
	ttl := d.cfg.NATProbe.CacheTTL
	if profile.Class == signalproto.NATUnknown && ttl > 30*time.Second {
		ttl = 30 * time.Second
	}
	d.profile = &cachedNATProfile{
		value:         profile,
		expiresAt:     now.Add(ttl),
		interfaceName: interfaceName,
		targets:       targets,
	}
	return profile, nil
}

func (d *Dialer) resetNATProfile() {
	d.profileMu.Lock()
	d.profile = nil
	d.profileFailures = 0
	d.profileMu.Unlock()
}

func (d *Dialer) recordProfileOutcome(err error) {
	d.profileMu.Lock()
	defer d.profileMu.Unlock()
	if err == nil {
		d.profileFailures = 0
		return
	}
	if !errors.Is(err, ErrPunchFailed) && !errors.Is(err, ErrSelectionFailed) {
		return
	}
	d.profileFailures++
	if d.profileFailures >= 2 {
		d.profile = nil
		d.profileFailures = 0
	}
}

func distinctProbeTargets(targets []string) int {
	seen := make(map[string]struct{})
	for _, target := range targets {
		hostPort, err := signalHostPort(target)
		if err == nil {
			seen[hostPort] = struct{}{}
		}
	}
	return len(seen)
}

func collectNATProfile(ctx context.Context, cfg Config) (signalproto.NATProfile, error) {
	interfaceName := cfg.interfaceName()
	tcpNetwork := cfg.tcpNetwork()
	bindAddress, err := bindIP(ctx, interfaceName, cfg.SignalAddr, tcpNetwork, cfg.Control, cfg.ResolveIP, cfg.InterfaceAddr, cfg.DebugLog)
	if err != nil {
		return signalproto.NATProfile{}, err
	}
	samples := make([]profileSample, 0, cfg.NATProbe.MaxSamples)
	for len(samples) < cfg.NATProbe.MaxSamples {
		block := cfg.NATProbe.InitialSamples
		if len(samples) > 0 {
			block = 8
		}
		if remaining := cfg.NATProbe.MaxSamples - len(samples); block > remaining {
			block = remaining
		}
		for range block {
			sample, sampleErr := collectProfileSample(ctx, cfg, bindAddress)
			if sampleErr != nil {
				return signalproto.NATProfile{}, sampleErr
			}
			samples = append(samples, sample)
		}
		profile := classifyNATProfile(samples, cfg.Predictive.PortScanRange, hasMultipleProbeIPs(cfg.NATProbe.Targets))
		if profile.Class != signalproto.NATUnknown {
			return profile, nil
		}
	}
	return classifyNATProfile(samples, cfg.Predictive.PortScanRange, hasMultipleProbeIPs(cfg.NATProbe.Targets)), nil
}

func collectProfileSample(ctx context.Context, cfg Config, bindAddress string) (profileSample, error) {
	tcpNetwork := cfg.tcpNetwork()
	localAddr, err := allocLocalPort(bindAddress, tcpNetwork, cfg.Control)
	if err != nil {
		return profileSample{}, err
	}
	targets := append([]string(nil), cfg.NATProbe.Targets...)
	mathrand.Shuffle(len(targets), func(left, right int) {
		targets[left], targets[right] = targets[right], targets[left]
	})
	session, err := newSessionID()
	if err != nil {
		return profileSample{}, err
	}
	client := &signalClient{mode: cfg.Mode, control: cfg.Control, resolveIP: cfg.ResolveIP, tcpNetwork: tcpNetwork}
	leases := make([]*mappingLease, 0, len(targets))
	defer func() {
		for _, lease := range leases {
			_ = lease.Close()
		}
	}()
	sample := profileSample{}
	for _, target := range targets {
		probeCtx, cancel := context.WithTimeout(ctx, cfg.NATProbe.Timeout)
		lease, probeErr := client.openMappingLease(probeCtx, target, localAddr, cfg.Room, "profile-"+session)
		cancel()
		if probeErr != nil {
			return profileSample{}, probeErr
		}
		leases = append(leases, lease)
		host, portText, splitErr := net.SplitHostPort(lease.Endpoint)
		if splitErr != nil {
			return profileSample{}, splitErr
		}
		port, convertErr := strconv.Atoi(portText)
		if convertErr != nil {
			return profileSample{}, convertErr
		}
		sample.publicIPs = append(sample.publicIPs, host)
		sample.ports = append(sample.ports, port)
	}
	return sample, nil
}

func classifyNATProfile(samples []profileSample, scanRange int, verifiedCrossIP bool) signalproto.NATProfile {
	profile := signalproto.NATProfile{Class: signalproto.NATUnknown, Samples: len(samples)}
	if len(samples) == 0 {
		return profile
	}
	publicIP := ""
	stableSamples := 0
	predictionHits := 0
	predictionTrials := 0
	threshold := max(scanRange/4, 1)
	for _, sample := range samples {
		if len(sample.ports) < 3 || len(sample.publicIPs) != len(sample.ports) {
			continue
		}
		sameIP := true
		for _, ip := range sample.publicIPs {
			if publicIP == "" {
				publicIP = ip
			}
			if ip != publicIP {
				sameIP = false
			}
		}
		if !sameIP {
			profile.PublicIP = publicIP
			return profile
		}
		stable := true
		for _, port := range sample.ports[1:] {
			if port != sample.ports[0] {
				stable = false
				break
			}
		}
		if stable {
			stableSamples++
		}
		step := circularPortDelta(sample.ports[0], sample.ports[1])
		predicted := wrapPort(sample.ports[1] + step)
		if circularPortDistance(predicted, sample.ports[2]) <= threshold {
			predictionHits++
		}
		predictionTrials++
	}
	profile.PublicIP = publicIP
	profile.PredictionHits = predictionHits
	profile.PredictionTrials = predictionTrials
	if stableSamples == len(samples) {
		profile.Class = signalproto.NATStable
		profile.VerifiedCrossIP = verifiedCrossIP
		return profile
	}
	if predictionTrials >= 8 && predictionHits*8 >= predictionTrials*7 {
		profile.Class = signalproto.NATSequential
		return profile
	}
	if predictionTrials >= 8 && predictionHits*2 <= predictionTrials {
		profile.Class = signalproto.NATDispersed
		return profile
	}
	return profile
}

func circularPortDelta(from, to int) int {
	delta := to - from
	if delta > 32767 {
		delta -= 65535
	} else if delta < -32767 {
		delta += 65535
	}
	return delta
}

func circularPortDistance(left, right int) int {
	delta := circularPortDelta(left, right)
	if delta < 0 {
		return -delta
	}
	return delta
}

func wrapPort(port int) int {
	for port < 1 {
		port += 65535
	}
	for port > 65535 {
		port -= 65535
	}
	return port
}

func hasMultipleProbeIPs(targets []string) bool {
	seen := make(map[string]struct{})
	for _, target := range targets {
		hostPort, err := signalHostPort(target)
		if err != nil {
			continue
		}
		host, _, err := net.SplitHostPort(hostPort)
		if err == nil {
			seen[host] = struct{}{}
		}
	}
	return len(seen) >= 2
}

// selectStrategy picks a concrete strategy for auto from the classified NAT
// profile. It takes plain values rather than the TCP Config so the UDP auto
// strategy can reuse it.
func selectStrategy(strategy Strategy, stableSide bool, profile signalproto.NATProfile) (Strategy, error) {
	if strategy != StrategyAuto {
		return strategy, nil
	}
	if !stableSide {
		return "", fmt.Errorf("Listener stable_side not enabled: %w", ErrUnsupportedNATPair)
	}
	switch profile.Class {
	case signalproto.NATStable:
		return StrategyDirect, nil
	case signalproto.NATSequential:
		return StrategyPredictive, nil
	case signalproto.NATDispersed:
		return StrategyBirthday, nil
	default:
		return "", ErrUnsupportedNATPair
	}
}
