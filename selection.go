package burrow

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"time"
)

const (
	selectionHello byte = 1
	selectionACK   byte = 2
	selectionSize       = 8 + 1 + 16 + 16 + sha256.Size
)

var selectionMagic = [8]byte{'B', 'R', 'S', 'E', 'L', '/', '1', 0}

func newSelectionKey() (string, error) {
	key := make([]byte, 32)
	if _, err := cryptorand.Read(key); err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

func clientSelectCandidate(ctx context.Context, conn net.Conn, session, encodedKey string) error {
	key, err := decodeSelectionKey(encodedKey)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := cryptorand.Read(nonce); err != nil {
		return err
	}
	clearDeadline, err := setSelectionDeadline(ctx, conn)
	if err != nil {
		return err
	}
	defer clearDeadline()
	if err := writeSelectionFrame(conn, selectionFrame(selectionHello, session, nonce, key)); err != nil {
		return err
	}
	frame := make([]byte, selectionSize)
	if _, err := io.ReadFull(conn, frame); err != nil {
		return err
	}
	return validateSelectionFrame(frame, selectionACK, session, nonce, key)
}

func serverSelectCandidate(ctx context.Context, conn net.Conn, session, encodedKey string) error {
	key, err := decodeSelectionKey(encodedKey)
	if err != nil {
		return err
	}
	clearDeadline, err := setSelectionDeadline(ctx, conn)
	if err != nil {
		return err
	}
	defer clearDeadline()
	frame := make([]byte, selectionSize)
	if _, err := io.ReadFull(conn, frame); err != nil {
		return err
	}
	nonce := append([]byte(nil), frame[25:41]...)
	if err := validateSelectionFrame(frame, selectionHello, session, nonce, key); err != nil {
		return err
	}
	return writeSelectionFrame(conn, selectionFrame(selectionACK, session, nonce, key))
}

func decodeSelectionKey(encoded string) ([]byte, error) {
	key, err := hex.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, ErrSelectionFailed
	}
	return key, nil
}

func selectionFrame(kind byte, session string, nonce, key []byte) []byte {
	frame := make([]byte, selectionSize)
	copy(frame[:8], selectionMagic[:])
	frame[8] = kind
	sessionHash := sha256.Sum256([]byte(session))
	copy(frame[9:25], sessionHash[:16])
	copy(frame[25:41], nonce)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(frame[:41])
	copy(frame[41:], mac.Sum(nil))
	return frame
}

func writeSelectionFrame(conn net.Conn, frame []byte) error {
	for len(frame) > 0 {
		written, err := conn.Write(frame)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		frame = frame[written:]
	}
	return nil
}

func validateSelectionFrame(frame []byte, kind byte, session string, nonce, key []byte) error {
	if len(frame) != selectionSize || !hmac.Equal(frame[:8], selectionMagic[:]) || frame[8] != kind {
		return ErrSelectionFailed
	}
	sessionHash := sha256.Sum256([]byte(session))
	if !hmac.Equal(frame[9:25], sessionHash[:16]) || !hmac.Equal(frame[25:41], nonce) {
		return ErrSelectionFailed
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(frame[:41])
	if !hmac.Equal(frame[41:], mac.Sum(nil)) {
		return ErrSelectionFailed
	}
	return nil
}

func setSelectionDeadline(ctx context.Context, conn net.Conn) (func(), error) {
	deadline := time.Now().Add(2 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if deadline.Before(time.Now()) {
		return nil, errors.Join(ErrSelectionFailed, context.DeadlineExceeded)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	return func() {
		stop()
		_ = conn.SetDeadline(time.Time{})
	}, nil
}
