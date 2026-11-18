package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/getkiko/burrow/internal/signalproto"
)

// normalizePrefix must map every spelling to the same exact path, otherwise
// client and server disagree about leading or trailing slashes.
func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":       "/",
		"/":      "/",
		"/abc":   "/abc",
		"abc":    "/abc",
		"/abc/":  "/abc",
		"  /abc": "/abc",
		"/a/b":   "/a/b",
	}
	for input, want := range cases {
		if got := normalizePrefix(input); got != want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

// effectiveQuery must accept both the plain URL query and the hidden cookie.
func TestEffectiveQuery(t *testing.T) {
	hidden := signalproto.EncodeHiddenQuery(url.Values{
		"command": []string{"nat"},
		"room":    []string{"cookie-room"},
	})

	t.Run("url only", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/abc?command=signal&room=url-room", nil)
		q := effectiveQuery(r)
		if q.Get("command") != "signal" || q.Get("room") != "url-room" {
			t.Fatalf("wrong URL values: %v", q)
		}
	})

	t.Run("cookie only", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/abc", nil)
		r.AddCookie(&http.Cookie{Name: signalproto.HideCookieName, Value: hidden})
		q := effectiveQuery(r)
		if q.Get("command") != "nat" || q.Get("room") != "cookie-room" {
			t.Fatalf("wrong cookie values: %v", q)
		}
	})

	t.Run("cookie wins over url", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/abc?command=signal&room=url-room", nil)
		r.AddCookie(&http.Cookie{Name: signalproto.HideCookieName, Value: hidden})
		q := effectiveQuery(r)
		if q.Get("command") != "nat" || q.Get("room") != "cookie-room" {
			t.Fatalf("cookie did not take precedence: %v", q)
		}
	})

	t.Run("garbage cookie falls back to url", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/abc?command=signal&room=url-room", nil)
		r.AddCookie(&http.Cookie{Name: signalproto.HideCookieName, Value: "!!!garbage!!!"})
		q := effectiveQuery(r)
		if q.Get("command") != "signal" || q.Get("room") != "url-room" {
			t.Fatalf("garbage cookie did not fall back to url: %v", q)
		}
	})
}

// An unknown or missing command must return 404 rather than reach a handler.
func TestHandleCommandUnknown(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	for _, target := range []string{"/abc", "/abc?command=", "/abc?command=bogus"} {
		recorder := httptest.NewRecorder()
		service.handleCommand(recorder, httptest.NewRequest("GET", target, nil))
		if recorder.Code != 404 {
			t.Errorf("%s: want 404, got %d", target, recorder.Code)
		}
	}
}

// command=health must return 204.
func TestHandleCommandHealth(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	recorder := httptest.NewRecorder()
	service.handleCommand(recorder, httptest.NewRequest("GET", "/abc?command=health", nil))
	if recorder.Code != 204 {
		t.Errorf("health: want 204, got %d", recorder.Code)
	}
}

// The signal, echo and nat commands must reach a WebSocket handler. A plain
// non-WebSocket GET is rejected by websocket.Accept (400), which is how it is
// told apart from the 404 of an unknown command.
func TestHandleCommandRoutesToWebSocket(t *testing.T) {
	service := newSignalService(newRegistry(), []byte("test-secret"))
	for _, command := range []string{"signal", "echo", "nat"} {
		recorder := httptest.NewRecorder()
		service.handleCommand(recorder, httptest.NewRequest("GET", "/abc?command="+command, nil))
		if recorder.Code == 404 {
			t.Errorf("command=%s was not routed, got 404", command)
		}
	}
}
