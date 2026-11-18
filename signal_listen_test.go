package burrow

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/signalproto"
)

// logRecorder collects formatted log lines from several goroutines.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) log(format string, args ...any) {
	r.mu.Lock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *logRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// A LISTEN the server rejects (here: rate_limited) must not be reported as a
// join. The listener logs the rejection, backs off, retries, and reports the
// join only once a LISTEN goes through.
func TestListenerHandlesListenRejection(t *testing.T) {
	const rejections = 2
	var mu sync.Mutex
	var listens []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			message, err := signalproto.Decode(data)
			if err != nil || message.Type != signalproto.TypeListen {
				continue
			}
			mu.Lock()
			listens = append(listens, time.Now())
			count := len(listens)
			mu.Unlock()
			if count <= rejections {
				reply, _ := signalproto.Encode(signalproto.Message{Type: signalproto.TypeError, Code: "rate_limited", Error: "listen rate exceeded"})
				_ = conn.Write(r.Context(), websocket.MessageText, reply)
			}
		}
	}))
	defer server.Close()

	var info, debug logRecorder
	addr := "ws://" + strings.TrimPrefix(server.URL, "http://")
	ctrl, err := newListenerCtrlWith(context.Background(), addr, "room-reject", "", nil, nil, 4, debug.log, info.log, "", 0, "tcp4")
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()

	deadline := time.Now().Add(10 * time.Second)
	for {
		joined := false
		for _, line := range debug.snapshot() {
			if strings.Contains(line, "joined room") {
				joined = true
			}
		}
		for _, line := range info.snapshot() {
			if strings.Contains(line, "joined room") {
				joined = true
			}
		}
		if joined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener never joined; info=%q debug=%q", info.snapshot(), debug.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	sent := append([]time.Time(nil), listens...)
	mu.Unlock()
	if len(sent) < rejections+1 {
		t.Fatalf("listener joined after only %d LISTEN messages", len(sent))
	}
	// Each rejection is followed by a backoff, not an immediate retry.
	if gap := sent[1].Sub(sent[0]); gap < 500*time.Millisecond {
		t.Fatalf("retry after a rejection came after %s, want a backoff", gap)
	}
	lines := info.snapshot()
	if len(lines) == 0 || !strings.Contains(lines[0], "cannot join room") || !strings.Contains(lines[0], "rate_limited") {
		t.Fatalf("first info line should report the rejection, got %q", lines)
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "joined room") || !strings.Contains(last, "after 2 failures") {
		t.Fatalf("join should be logged after the rejections, got %q", lines)
	}
}

func TestIsListenRejection(t *testing.T) {
	for _, code := range []string{"rate_limited", "too_many_rooms", "invalid_room"} {
		if !isListenRejection(signalproto.Message{Type: signalproto.TypeError, Code: code}) {
			t.Fatalf("%s without a session should be a LISTEN rejection", code)
		}
	}
	if isListenRejection(signalproto.Message{Type: signalproto.TypeError, Code: "rate_limited", Session: "s1"}) {
		t.Fatal("a DIAL error carries a session and is not a LISTEN rejection")
	}
	if isListenRejection(signalproto.Message{Type: signalproto.TypeError, Code: "bad_message"}) {
		t.Fatal("bad_message is not a LISTEN rejection")
	}
}

// When the signal server keeps refusing the connection (here HTTP 429), the
// Dialer's eventual timeout must carry that refusal instead of a bare
// context error.
func TestSignalDialReportsLastConnectError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many concurrent signal connections", http.StatusTooManyRequests)
	}))
	defer server.Close()

	addr := "ws://" + strings.TrimPrefix(server.URL, "http://")
	ctrl := newSignalControl(addr, "room-429", "", nil, nil, 1, "tcp4")
	defer ctrl.close()

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	_, err := ctrl.dial(ctx, signalproto.Message{Room: "room-429", Session: "sess-429"})
	if err == nil {
		t.Fatal("dial should fail")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error should still match the context deadline: %v", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("error should report the last refusal (429): %v", err)
	}
}
