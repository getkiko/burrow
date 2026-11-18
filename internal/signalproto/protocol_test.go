package signalproto

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestHiddenQueryRoundTrip(t *testing.T) {
	original := url.Values{
		"command": []string{"nat"},
		"room":    []string{"r"},
		"session": []string{"s"},
	}
	encoded := EncodeHiddenQuery(original)

	// The encoding must only use characters legal in a cookie value
	// (unpadded base64url: A-Z a-z 0-9 - _).
	for _, r := range encoded {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			t.Fatalf("encoding contains illegal cookie character %q: %s", r, encoded)
		}
	}

	decoded, err := DecodeHiddenQuery(encoded)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	for key := range original {
		if decoded.Get(key) != original.Get(key) {
			t.Fatalf("round trip mismatch for key=%s: got %q want %q", key, decoded.Get(key), original.Get(key))
		}
	}
}

func TestDecodeHiddenQueryRejectsGarbage(t *testing.T) {
	if _, err := DecodeHiddenQuery("!!!not base64!!!"); err == nil {
		t.Fatal("invalid base64 should return an error")
	}
}

func TestDecodeHiddenQueryEmpty(t *testing.T) {
	values, err := DecodeHiddenQuery("")
	if err != nil {
		t.Fatalf("empty string should decode to empty Values, not an error: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("empty string should decode to empty Values, got %v", values)
	}
}

// A message that omits transport must be accepted and normalized to tcp.
func TestDecodeNormalizesMissingTransport(t *testing.T) {
	raw := `{"version":1,"type":"listen","room":"r"}`
	message, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("message without transport should be accepted: %v", err)
	}
	if message.Transport != TransportTCP {
		t.Fatalf("Transport should be normalized to tcp, got %q", message.Transport)
	}
}

// Any version other than Version must be rejected.
func TestDecodeRejectsOtherVersions(t *testing.T) {
	for _, version := range []int{0, Version + 1, 4, 5} {
		raw, _ := json.Marshal(Message{Version: version, Type: TypeListen, Room: "r"})
		if _, err := Decode(raw); err == nil {
			t.Fatalf("version=%d should be rejected", version)
		}
	}
}

// An explicit transport=udp must be preserved, not normalized.
func TestDecodeExplicitTransportPreserved(t *testing.T) {
	raw, _ := json.Marshal(Message{Version: Version, Type: TypeListen, Room: "r", Transport: TransportUDP})
	message, err := Decode(raw)
	if err != nil {
		t.Fatalf("udp message should be accepted: %v", err)
	}
	if message.Transport != TransportUDP {
		t.Fatalf("explicit udp should not be normalized, got %q", message.Transport)
	}
}

// Encode always stamps the current Version, whatever the caller set.
func TestEncodeSetsVersion(t *testing.T) {
	data, err := Encode(Message{Version: 99, Type: TypeListen, Room: "r", Transport: TransportUDP, Proof: "p", Capacity: 8})
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if decoded.Version != Version {
		t.Fatalf("Encode should set version %d, got %d", Version, decoded.Version)
	}
	if decoded.Transport != TransportUDP || decoded.Proof != "p" || decoded.Capacity != 8 {
		t.Fatalf("Encode dropped fields: %+v", decoded)
	}
}

// With identical parameters, an omitted transport and an explicit tcp must
// produce the same fingerprint as Fingerprint(); otherwise peers that omit
// the field would be reported as a strategy mismatch.
func TestFingerprintForTransportTCPMatchesFingerprint(t *testing.T) {
	params := StrategyParameters{PredictiveRange: 512}
	want := params.Fingerprint()
	if got := params.FingerprintForTransport(""); got != want {
		t.Fatalf("empty transport fingerprint should equal Fingerprint(): got %q want %q", got, want)
	}
	if got := params.FingerprintForTransport(TransportTCP); got != want {
		t.Fatalf("tcp fingerprint should equal Fingerprint(): got %q want %q", got, want)
	}
}

// The transport is part of the fingerprint: udp and tcp with identical
// parameters must differ, otherwise transport isolation is meaningless.
func TestFingerprintForTransportUDPDiffersFromTCP(t *testing.T) {
	params := StrategyParameters{PredictiveRange: 512}
	if params.FingerprintForTransport(TransportUDP) == params.FingerprintForTransport(TransportTCP) {
		t.Fatal("udp and tcp fingerprints should differ")
	}
}

// Golden values pin the fingerprint algorithm: changing it would break
// compatibility between peers built from different releases.
func TestFingerprintGolden(t *testing.T) {
	params := StrategyParameters{PredictiveRange: 512}
	if got, want := params.FingerprintForTransport(TransportTCP), "550bdd41dc76fff2"; got != want {
		t.Fatalf("tcp fingerprint = %q, want %q", got, want)
	}
	if got, want := params.FingerprintForTransport(TransportUDP), "2ad69eb0604a96dc"; got != want {
		t.Fatalf("udp fingerprint = %q, want %q", got, want)
	}
}

func TestValidateRoom(t *testing.T) {
	for _, room := range []string{"demo", "a", strings.Repeat("r", MaxRoomLength), " padded "} {
		if err := ValidateRoom(room); err != nil {
			t.Fatalf("ValidateRoom(%q): %v", room, err)
		}
	}
	for _, room := range []string{"", "   ", "\t\n", strings.Repeat("r", MaxRoomLength+1)} {
		if err := ValidateRoom(room); err == nil {
			t.Fatalf("ValidateRoom(%q) should fail", room)
		}
	}
	if !ValidName(strings.Repeat("s", MaxSessionLength), MaxSessionLength) || ValidName(strings.Repeat("s", MaxSessionLength+1), MaxSessionLength) {
		t.Fatal("ValidName must accept exactly max bytes and reject more")
	}
}
