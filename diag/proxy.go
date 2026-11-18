package diag

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// connectDialer returns a DialContext that establishes TCP connections
// through an HTTP CONNECT proxy.
//
// http.Transport.Proxy is not usable here: the standard library only sends
// CONNECT for https requests, while http/ws requests are sent as an
// absolute-URI GET for the proxy to forward. WebSocket needs a transparent
// TCP tunnel; with an absolute-URI request the proxy treats it as plain
// HTTP, the Upgrade handshake never completes and the read blocks until the
// deadline ("failed to get reader: context deadline exceeded").
//
// So CONNECT is sent explicitly: connect to the proxy, send
// "CONNECT host:port HTTP/1.1", and once the proxy answers 200 the TCP
// connection is a transparent tunnel to the target on which the WebSocket
// handshake runs.
//
// base is the dialer used to reach the proxy. Interface binding and other
// controls apply to that hop, which must still use the correct physical
// interface (the proxy is on a private network and unreachable otherwise).
func connectDialer(base *net.Dialer, proxyAddr string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		conn, err := base.DialContext(ctx, network, proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("connect to proxy %s: %w", proxyAddr, err)
		}

		// Close the connection when ctx is cancelled; otherwise reading the
		// response below could block until the TCP timeout.
		done := make(chan struct{})
		defer close(done)
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-done:
			}
		}()

		request := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Opaque: target},
			Host:   target,
			Header: make(http.Header),
		}
		if err := request.Write(conn); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("send CONNECT: %w", err)
		}

		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("read CONNECT response: %w", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			_ = conn.Close()
			return nil, fmt.Errorf("proxy rejected CONNECT %s: %s", target, response.Status)
		}

		// bufio may have read ahead into tunnel data; if so, the raw conn
		// cannot be returned as is.
		if reader.Buffered() > 0 {
			return &bufferedConn{Conn: conn, reader: reader}, nil
		}
		return conn, nil
	}
}

// bufferedConn returns bytes read ahead into bufio during the CONNECT
// handshake to subsequent readers.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// normalizeProxyAddr normalizes a user-supplied proxy address to host:port.
//
// It accepts "10.0.0.200:80", "http://10.0.0.200:80" and "10.0.0.200"
// (port 80 by default), since addresses copied from system APN settings
// come in inconsistent formats.
func normalizeProxyAddr(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimSuffix(value, "/")
	if !strings.Contains(value, ":") {
		return net.JoinHostPort(value, "80")
	}
	return value
}
