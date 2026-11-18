package burrow

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/getkiko/burrow/internal/signalproto"
)

// signalHostPort adds the default port to ws/wss addresses without one;
// otherwise bindIP's fallback path fails with "missing port in address".
func TestSignalHostPort(t *testing.T) {
	cases := []struct {
		addr    string
		want    string
		wantErr bool
	}{
		{addr: "ws://signal.example.com", want: "signal.example.com:80"},
		{addr: "wss://signal.example.com", want: "signal.example.com:443"},
		{addr: "ws://signal.example.com:9960", want: "signal.example.com:9960"},
		{addr: "wss://example.com:8443", want: "example.com:8443"},
		{addr: "1.2.3.4:9960", want: "1.2.3.4:9960"},
		{addr: "ws://[2001:db8::1]:9960", want: "[2001:db8::1]:9960"},
		{addr: "ws://[2001:db8::1]", want: "[2001:db8::1]:80"},
		{addr: "no-port-no-scheme", wantErr: true},
		{addr: "ftp://example.com", wantErr: true},
	}
	for _, tc := range cases {
		got, err := signalHostPort(tc.addr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("signalHostPort(%q) want error, got %q", tc.addr, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("signalHostPort(%q) unexpected error: %v", tc.addr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("signalHostPort(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// signalURL adds the scheme, appends ?command=xxx and keeps the address path
// as a fixed prefix (ws://host/abc -> ws://host/abc?command=echo).
func TestSignalURL(t *testing.T) {
	cases := []struct{ addr, command, want string }{
		// No prefix: the root path.
		{"signal.example.com:9960", "signal", "ws://signal.example.com:9960/?command=signal"},
		{"ws://signal.example.com", "echo", "ws://signal.example.com/?command=echo"},
		{"wss://example.com:8443", "signal", "wss://example.com:8443/?command=signal"},
		// With a prefix: kept as is before the query.
		{"ws://signal.example.com:9960/abc", "echo", "ws://signal.example.com:9960/abc?command=echo"},
		{"wss://example.com:8443/a/b", "nat", "wss://example.com:8443/a/b?command=nat"},
		// Trailing and missing leading slashes are normalized.
		{"ws://example.com/abc/", "signal", "ws://example.com/abc?command=signal"},
		{"ws://example.com/", "signal", "ws://example.com/?command=signal"},
	}
	for _, tc := range cases {
		got, err := signalURL(tc.addr, signalParams(tc.command))
		if err != nil {
			t.Errorf("signalURL(%q): %v", tc.addr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("signalURL(%q,%q) = %q, want %q", tc.addr, tc.command, got, tc.want)
		}
	}
	if _, err := signalURL("ftp://example.com", signalParams("signal")); err == nil {
		t.Error("a non-ws/wss scheme should fail")
	}
}

// Extra query values (room/session) coexist with command and do not
// override it.
func TestSignalURLWithQuery(t *testing.T) {
	got, err := signalURL("ws://example.com/abc", signalParams("nat", url.Values{
		"room":    []string{"r1"},
		"session": []string{"s1"},
	}))
	if err != nil {
		t.Fatalf("signalURL: %v", err)
	}
	want := "ws://example.com/abc?command=nat&room=r1&session=s1"
	if got != want {
		t.Errorf("signalURL = %q, want %q", got, want)
	}
}

// In header mode signalURL gets an empty query and keeps only the prefix
// path, so the URL carries no parameters.
func TestSignalURLHiddenNoQuery(t *testing.T) {
	got, err := signalURL("ws://example.com/abc", nil)
	if err != nil {
		t.Fatalf("signalURL: %v", err)
	}
	if got != "ws://example.com/abc" {
		t.Errorf("header mode URL should have no query, got %q", got)
	}
}

// host:port is still extracted from an address with a prefix; otherwise
// bindIP cannot find the bind address.
func TestSignalHostPortWithPrefix(t *testing.T) {
	cases := map[string]string{
		"ws://signal.example.com:9960/abc": "signal.example.com:9960",
		"wss://example.com/a/b":            "example.com:443",
		"ws://example.com/abc":             "example.com:80",
	}
	for addr, want := range cases {
		got, err := signalHostPort(addr)
		if err != nil {
			t.Errorf("signalHostPort(%q): %v", addr, err)
			continue
		}
		if got != want {
			t.Errorf("signalHostPort(%q) = %q, want %q", addr, got, want)
		}
	}
}

// Edge cases of signalproto.ValidateEndpoint.
func TestValidateHostPort(t *testing.T) {
	ok := []string{"1.2.3.4:1", "1.2.3.4:65535", "[2001:db8::1]:443"}
	bad := []string{"1.2.3.4:0", "1.2.3.4:65536", "example.com:80", "1.2.3.4", "1.2.3.4:abc"}
	for _, s := range ok {
		if err := signalproto.ValidateEndpoint(s); err != nil {
			t.Errorf("ValidateEndpoint(%q) should pass, got %v", s, err)
		}
	}
	for _, s := range bad {
		if err := signalproto.ValidateEndpoint(s); err == nil {
			t.Errorf("ValidateEndpoint(%q) should fail", s)
		}
	}
}

// The dialSignalWebSocket handshake strips the port from the Host header and
// sends the fixed desktop Chrome User-Agent instead of Go's default
// "Go-http-client/1.1".
func TestDialSignalWebSocketHeaders(t *testing.T) {
	captured := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer server.Close()

	// server.URL looks like http://127.0.0.1:PORT; pass it as ws:// so the
	// address has a port for the Host stripping to act on.
	wsAddr := "ws://" + strings.TrimPrefix(server.URL, "http://")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialSignalWebSocket(ctx, wsAddr, signalproto.CommandEcho, nil, "", nil, nil, "", "tcp4")
	if err != nil {
		t.Fatalf("dialSignalWebSocket: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	req := <-captured
	// Host has no port: httptest listens on 127.0.0.1:PORT, so Host must be
	// just "127.0.0.1".
	if host, _, splitErr := net.SplitHostPort(req.Host); splitErr == nil {
		t.Errorf("Host header still has a port: %q (host=%q)", req.Host, host)
	}
	if req.Host != "127.0.0.1" {
		t.Errorf("Host header should be 127.0.0.1, got %q", req.Host)
	}
	if ua := req.Header.Get("User-Agent"); ua != signalUserAgent {
		t.Errorf("User-Agent not overridden: %q", ua)
	}
	const wantUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	if signalUserAgent != wantUA {
		t.Errorf("signalUserAgent = %q, want desktop Chrome %q", signalUserAgent, wantUA)
	}
}

func TestDialSignalTCPFiltersIPv6ResolverResults(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	resolver := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("127.0.0.1"),
			netip.MustParseAddr("::1"),
		}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialSignalTCP(ctx, &net.Dialer{}, net.JoinHostPort("signal.test", strconv.Itoa(port)), resolver, "tcp6")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	select {
	case serverConn := <-accepted:
		defer serverConn.Close()
		if host, _, splitErr := net.SplitHostPort(serverConn.RemoteAddr().String()); splitErr != nil || host != "::1" {
			t.Fatalf("server saw source address %q, err=%v", serverConn.RemoteAddr(), splitErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// Header mode puts the parameters in a Cookie, leaves the URL query empty,
// and the server-side decoder recovers command from the Cookie.
func TestDialSignalWebSocketHeaderMode(t *testing.T) {
	captured := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer server.Close()

	wsAddr := "ws://" + strings.TrimPrefix(server.URL, "http://") + "/signal"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := url.Values{"room": []string{"r1"}, "session": []string{"s1"}}
	conn, err := dialSignalWebSocket(ctx, wsAddr, signalproto.CommandNAT, query, "header", nil, nil, "", "tcp4")
	if err != nil {
		t.Fatalf("dialSignalWebSocket: %v", err)
	}
	conn.Close(websocket.StatusNormalClosure, "")

	req := <-captured
	// The URL carries no parameters.
	if req.URL.RawQuery != "" {
		t.Errorf("header mode URL still has a query: %q", req.URL.RawQuery)
	}
	// The Cookie exists and decodes to all parameters.
	cookie, err := req.Cookie(signalproto.HideCookieName)
	if err != nil {
		t.Fatalf("missing Cookie %q: %v", signalproto.HideCookieName, err)
	}
	values, err := signalproto.DecodeHiddenQuery(cookie.Value)
	if err != nil {
		t.Fatalf("decode Cookie: %v", err)
	}
	if values.Get("command") != signalproto.CommandNAT {
		t.Errorf("Cookie command = %q, want %q", values.Get("command"), signalproto.CommandNAT)
	}
	if values.Get("room") != "r1" || values.Get("session") != "s1" {
		t.Errorf("Cookie lost room/session: %v", values)
	}
}
