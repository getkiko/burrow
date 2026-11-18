package main

import (
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

const probeTokenTTL = 30 * time.Second

type tokenClaims struct {
	Endpoint string `json:"endpoint"`
	Room     string `json:"room"`
	Session  string `json:"session"`
	Expires  int64  `json:"expires"`
}

type tokenSigner struct {
	secret []byte
}

func newTokenSigner(secret []byte) *tokenSigner {
	if len(secret) == 0 {
		secret = make([]byte, 32)
		_, _ = cryptorand.Read(secret)
	}
	return &tokenSigner{secret: append([]byte(nil), secret...)}
}

func (s *tokenSigner) Issue(endpoint, room, session string, now time.Time) (string, int64) {
	claims := tokenClaims{Endpoint: endpoint, Room: room, Session: session, Expires: now.Add(probeTokenTTL).Unix()}
	payload, _ := json.Marshal(claims)
	signature := s.sign(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), claims.Expires
}

func (s *tokenSigner) Verify(token, endpoint, room, session, remoteIP string, now time.Time) error {
	var payloadPart, signaturePart string
	for index := range token {
		if token[index] == '.' {
			payloadPart, signaturePart = token[:index], token[index+1:]
			break
		}
	}
	if payloadPart == "" || signaturePart == "" {
		return errors.New("malformed probe token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return err
	}
	expected := s.sign(payload)
	if len(signature) != len(expected) || subtle.ConstantTimeCompare(signature, expected) != 1 {
		return errors.New("invalid probe token signature")
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return err
	}
	if claims.Endpoint != endpoint || claims.Room != room || claims.Session != session {
		return errors.New("probe token scope mismatch")
	}
	if now.Unix() > claims.Expires {
		return errors.New("probe token expired")
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil || host != remoteIP {
		return errors.New("probe token source mismatch")
	}
	return nil
}

func (s *tokenSigner) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

type rateBucket struct {
	tokens float64
	at     time.Time
}

type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
	rate    float64
	burst   float64
}

func newIPRateLimiter(rate, burst int) *ipRateLimiter {
	return &ipRateLimiter{buckets: make(map[string]rateBucket), rate: float64(rate), burst: float64(burst)}
}

func (l *ipRateLimiter) Allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, exists := l.buckets[ip]
	if !exists {
		bucket = rateBucket{tokens: l.burst, at: now}
	}
	bucket.tokens += now.Sub(bucket.at).Seconds() * l.rate
	if bucket.tokens > l.burst {
		bucket.tokens = l.burst
	}
	bucket.at = now
	if bucket.tokens < 1 {
		l.buckets[ip] = bucket
		return false
	}
	bucket.tokens--
	l.buckets[ip] = bucket
	return true
}

// Prune drops buckets that have been idle long enough to refill completely.
// Such a bucket behaves exactly like a missing one, so removing it changes
// no decision and keeps the map from growing with every source address seen.
func (l *ipRateLimiter) Prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for ip, bucket := range l.buckets {
		if now.Sub(bucket.at) >= full {
			delete(l.buckets, ip)
		}
	}
}

func (l *ipRateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ipConnLimiter caps concurrent connections per source IP. A limit of zero
// or less disables it.
type ipConnLimiter struct {
	mu    sync.Mutex
	limit int
	conns map[string]int
}

func newIPConnLimiter(limit int) *ipConnLimiter {
	return &ipConnLimiter{limit: limit, conns: make(map[string]int)}
}

// Acquire reserves a connection slot for ip and reports whether one was
// available. Every successful Acquire must be paired with Release.
func (l *ipConnLimiter) Acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit > 0 && l.conns[ip] >= l.limit {
		return false
	}
	l.conns[ip]++
	return true
}

func (l *ipConnLimiter) Release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns[ip] <= 1 {
		delete(l.conns, ip)
		return
	}
	l.conns[ip]--
}

// limitKey returns the key per-source limits are counted under. An IPv4
// address (including an IPv4-mapped IPv6 address) counts as itself; an IPv6
// address counts as its /64, because a single host or subscriber usually
// controls a whole /64 and could otherwise pick a fresh address per request.
// Token source checks and the address echo keep using the full address.
func limitKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is4() {
		return addr.String()
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}
