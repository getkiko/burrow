package burrow

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// sessionIDBytes is the session ID length. 128 bits makes collisions
// negligible at any realistic concurrency; pairing correctness relies on
// session IDs being unique.
const sessionIDBytes = 16

// newSessionID returns a random hex-encoded session ID.
func newSessionID() (string, error) {
	b := make([]byte, sessionIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}
