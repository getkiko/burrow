package burrow

import (
	"context"
	"net"
	"testing"
	"time"
)

// collectUDPProfileSample must query each STUN target in order and record
// each mapping in that order. The three mock servers return different
// mapped ports so that both the order and the target-to-result mapping are
// checked.
func TestCollectUDPProfileSampleOrdered(t *testing.T) {
	ports := []int{40001, 40002, 40003}
	var servers []string
	for _, port := range ports {
		server := startMockSTUNServer(t, &net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: port})
		servers = append(servers, server.addr)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sample, err := collectUDPProfileSample(context.Background(), conn, servers, 2*time.Second)
	if err != nil {
		t.Fatalf("collectUDPProfileSample failed: %v", err)
	}
	if len(sample.ports) != len(ports) || len(sample.publicIPs) != len(ports) {
		t.Fatalf("sample length mismatch: got ports=%v ips=%v, want %d entries", sample.ports, sample.publicIPs, len(ports))
	}
	for i, want := range ports {
		if sample.ports[i] != want {
			t.Errorf("target %d mapped port mismatch: got %d, want %d (order broken?)", i, sample.ports[i], want)
		}
		if sample.publicIPs[i] != "203.0.113.5" {
			t.Errorf("target %d mapped IP mismatch: got %s", i, sample.publicIPs[i])
		}
	}
}

// If one target fails, collectUDPProfileSample must fail as a whole rather
// than return a half-filled profileSample.
func TestCollectUDPProfileSampleFailsOnUnreachableTarget(t *testing.T) {
	good := startMockSTUNServer(t, &net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 40001})
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	servers := []string{good.addr, "192.0.2.1:3478"}
	_, err = collectUDPProfileSample(context.Background(), conn, servers, 500*time.Millisecond)
	if err == nil {
		t.Fatal("want an error because the second target is unreachable, got nil")
	}
}

// When every sample returns the same mapping, collectUDPProfile must
// classify the NAT as stable. The thresholds belong to classifyNATProfile;
// this only checks that the UDP collection layer feeds it correctly.
func TestCollectUDPProfileStable(t *testing.T) {
	// Three different STUN targets returning the same mapping, as with an
	// endpoint-independent (cone) NAT.
	mapped := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 55555}
	var servers []string
	for range 3 {
		server := startMockSTUNServer(t, mapped)
		servers = append(servers, server.addr)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	profile, err := collectUDPProfile(context.Background(), conn, servers, 2*time.Second, 3, 8, 512)
	if err != nil {
		t.Fatalf("collectUDPProfile failed: %v", err)
	}
	if profile.Class != "stable" {
		t.Fatalf("want class stable, got %s (profile=%+v)", profile.Class, profile)
	}
}
