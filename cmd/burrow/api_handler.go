package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var apiIndexHTML []byte

type apiHTTPServer struct {
	catalog apiCatalog
	runs    *apiRunManager
	mux     *http.ServeMux
}

type apiRunRequest struct {
	Arguments map[string]any `json:"arguments"`
}

func newAPIHTTPServer() (*apiHTTPServer, error) {
	server := &apiHTTPServer{
		catalog: newAPICatalog(),
		runs:    newAPIRunManager(),
		mux:     http.NewServeMux(),
	}
	server.routes()
	return server, nil
}

func (server *apiHTTPServer) routes() {
	server.mux.HandleFunc("/", server.handleIndex)
	server.mux.HandleFunc("/api", server.handleAPIIndex)
	server.mux.HandleFunc("/api/commands", server.handleCommands)
	server.mux.HandleFunc("/api/runs", server.handleRuns)
	server.mux.HandleFunc("/api/runs/", server.handleRun)
	for _, tab := range server.catalog.Tabs {
		for _, command := range tab.Commands {
			spec := command
			server.mux.HandleFunc(spec.Endpoint, func(w http.ResponseWriter, r *http.Request) {
				server.handleStartRun(w, r, spec)
			})
		}
	}
}

func (server *apiHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'")
	if status, err := checkAPIRequest(r); err != nil {
		writeAPIError(w, status, err)
		return
	}
	server.mux.ServeHTTP(w, r)
}

// checkAPIRequest protects the local console from other web pages. The API
// can start network operations and returns their output, so:
//
//   - the Host header must name a loopback address or localhost, which
//     defeats DNS rebinding (a rebound name still sends its own Host);
//   - a request carrying an Origin header must come from the same origin as
//     the Host, so a cross-site page cannot read or drive the API;
//   - a state-changing request must declare Content-Type application/json,
//     which a cross-site form or a CORS "simple" request cannot send without
//     a preflight.
//
// The embedded console is served from the same origin and sends JSON, so it
// passes all three checks.
func checkAPIRequest(r *http.Request) (int, error) {
	if !isLoopbackHost(r.Host) {
		return http.StatusForbidden, fmt.Errorf("host %q is not allowed; use localhost or a loopback address", r.Host)
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		return http.StatusForbidden, fmt.Errorf("cross-origin request from %q is not allowed", origin)
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return 0, nil
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return http.StatusUnsupportedMediaType, fmt.Errorf("content type must be application/json")
	}
	return 0, nil
}

// isLoopbackHost reports whether a Host header value, with or without a
// port, names localhost or a loopback IP address.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

// sameOrigin reports whether origin is the http origin of host.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

func (server *apiHTTPServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(apiIndexHTML)
	}
}

func (server *apiHTTPServer) handleAPIIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "burrow api",
		"endpoints": []string{
			"GET /api/commands",
			"POST /api/tcpnat",
			"POST /api/stun",
			"POST /api/punch/client",
			"POST /api/punch/server",
			"POST /api/birthday/client",
			"POST /api/birthday/server",
			"GET /api/runs",
			"GET /api/runs/{id}",
			"GET /api/runs/{id}/events",
			"DELETE /api/runs/{id}",
		},
	})
}

func (server *apiHTTPServer) handleCommands(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, server.catalog)
}

func (server *apiHTTPServer) handleStartRun(w http.ResponseWriter, r *http.Request, spec apiCommandSpec) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	var request apiRunRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Errorf("invalid request json: %w", err))
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	if request.Arguments == nil {
		request.Arguments = make(map[string]any)
	}
	values, err := normalizeAPICommandValues(spec, request.Arguments)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	view, err := server.runs.start(spec, values)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, view)
}

func (server *apiHTTPServer) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/runs" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": server.runs.list()})
}

func (server *apiHTTPServer) handleRun(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/runs/"), "/")
	parts := strings.Split(path, "/")
	if path == "" || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	run, exists := server.runs.get(parts[0])
	if !exists {
		writeAPIError(w, http.StatusNotFound, fmt.Errorf("run %s not found", parts[0]))
		return
	}
	if len(parts) == 2 {
		if parts[1] != "events" {
			http.NotFound(w, r)
			return
		}
		server.handleRunEvents(w, r, run)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, run.snapshot(true))
	case http.MethodDelete:
		view, err := server.runs.stop(parts[0])
		if err != nil {
			writeAPIError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writeMethodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

func (server *apiHTTPServer) handleRunEvents(w http.ResponseWriter, r *http.Request, run *apiRun) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, fmt.Errorf("http writer does not support streaming"))
		return
	}
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		view, chunk, nextCursor, notify := run.readFrom(cursor)
		if chunk != "" {
			if err := writeSSE(w, "output", map[string]any{"chunk": chunk, "cursor": nextCursor}); err != nil {
				return
			}
			cursor = nextCursor
		}
		if err := writeSSE(w, "state", view); err != nil {
			return
		}
		flusher.Flush()
		if isTerminalRunStatus(view.Status) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-notify:
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w io.Writer, event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeMethodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeAPIError(w, http.StatusMethodNotAllowed, fmt.Errorf("http method not allowed"))
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("request must contain a single json object")
		}
		return fmt.Errorf("invalid request json: %w", err)
	}
	return nil
}
