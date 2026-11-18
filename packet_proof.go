package burrow

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// UDP session proof hooks. A proof authenticates the signaling messages of
// a session. It protects the signaling plane only and is independent of any
// authentication the upper-layer protocol performs on the data plane.
//
// Key derivation (HKDF-SHA256 of the password with info proofHKDFInfo) and
// the canonical encoding are implemented by NewProofSigner and
// NewProofVerifier. Callers build both hooks from a shared password and set
// them on PacketConfig.

// ProofContext is the set of signaling fields a proof covers: transport,
// room, session, endpoint, profile, strategy and fingerprint. It is a plain
// value type so the hook signatures do not depend on
// internal/signalproto.Message.
type ProofContext struct {
	Transport   string
	Room        string
	Session     string
	Endpoint    string
	Strategy    Strategy
	Fingerprint string
	// Profile is the serialized local UDP NAT profile for the auto strategy.
	// It is empty when no profile was collected or the strategy is not
	// auto. Its canonical encoding is up to the implementation.
	Profile string
}

// ProofSigner produces the proof for a signaling message. Returning an
// empty string attaches no proof, as if the hook were not set.
type ProofSigner func(ctx ProofContext) (string, error)

// ProofVerifier checks the proof carried by a peer's signaling message. It
// is called even when the proof is empty, so an implementation can require
// one. A bad proof must produce a uniform authentication error and must not
// trigger port scanning or candidate socket allocation.
type ProofVerifier func(ctx ProofContext, proof string) error

// ErrProofMismatch is the uniform authentication error NewProofVerifier
// returns when a proof is missing or wrong. Missing and wrong proofs are
// not distinguished, so an observer cannot tell them apart.
var ErrProofMismatch = errors.New("burrow: signal proof verification failed")

const proofHKDFInfo = "burrow/signal/v1"

// deriveSignalKey derives the 32-byte signaling key from password:
// HKDF-SHA256(password, info=proofHKDFInfo).
func deriveSignalKey(password string) []byte {
	r := hkdf.New(sha256.New, []byte(password), nil, []byte(proofHKDFInfo))
	key := make([]byte, 32)
	_, _ = io.ReadFull(r, key)
	return key
}

// canonicalProof encodes a ProofContext as a deterministic byte string.
// Fields are separated by \x00 so that adjacent fields cannot be confused
// (room="ab", session="c" versus room="a", session="bc"). It covers
// transport, room, session, endpoint, strategy, fingerprint and profile.
func canonicalProof(ctx ProofContext) []byte {
	return []byte(strings.Join([]string{
		ctx.Transport,
		ctx.Room,
		ctx.Session,
		ctx.Endpoint,
		string(ctx.Strategy),
		ctx.Fingerprint,
		ctx.Profile,
	}, "\x00"))
}

// NewProofSigner returns a ProofSigner that signs signaling messages with a
// key derived from password:
// proof = hex(HMAC-SHA256(signalKey, canonical(ctx))[:16]).
func NewProofSigner(password string) ProofSigner {
	key := deriveSignalKey(password)
	return func(ctx ProofContext) (string, error) {
		mac := hmac.New(sha256.New, key)
		mac.Write(canonicalProof(ctx))
		return hex.EncodeToString(mac.Sum(nil)[:16]), nil
	}
}

// NewProofVerifier returns a ProofVerifier that checks peer proofs with a
// key derived from password. A missing or wrong proof yields
// ErrProofMismatch. The comparison is constant time (hmac.Equal).
func NewProofVerifier(password string) ProofVerifier {
	key := deriveSignalKey(password)
	return func(ctx ProofContext, proof string) error {
		got, err := hex.DecodeString(proof)
		if err != nil {
			return ErrProofMismatch
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(canonicalProof(ctx))
		if !hmac.Equal(got, mac.Sum(nil)[:16]) {
			return ErrProofMismatch
		}
		return nil
	}
}
