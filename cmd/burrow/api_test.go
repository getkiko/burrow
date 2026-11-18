package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkiko/burrow/diag"
)

func TestAPIIndexAndCommandCatalog(t *testing.T) {
	server, err := newAPIHTTPServer()
	if err != nil {
		t.Fatal(err)
	}

	indexResponse := httptest.NewRecorder()
	server.ServeHTTP(indexResponse, newLocalRequest(http.MethodGet, "/", nil))
	if indexResponse.Code != http.StatusOK {
		t.Fatalf("index status = %d", indexResponse.Code)
	}
	index := indexResponse.Body.String()
	for _, expected := range []string{
		"TCP NAT",
		"STUN",
		"Punch",
		"Birthday",
		"* required",
		"role-switch",
		"Save profile",
		"Saved profiles",
		"burrow.cli.profiles.v1",
		"mergeLegacySignalPath",
		"shouldRenderField",
		"Copy log",
		"copyOutput",
		"isConsoleNearBottom",
		"Home/office network (NAT)",
		"Public host/port forward (fixed address)",
		"NAT mapping types",
		"Endpoint-Independent Mapping",
		"Address-Dependent Mapping",
		"Address and Port-Dependent Mapping",
		"second public IP",
		"Burrow Console",
		diag.DefaultSTUNServer,
		"isTerminalRunStatus(state.currentRun.status)",
	} {
		if !strings.Contains(index, expected) {
			t.Fatalf("index missing %q", expected)
		}
	}
	if strings.Contains(index, `id="loadProfileButton"`) {
		t.Fatal("saved profiles must load directly from the select")
	}
	if got := indexResponse.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("index missing content security policy")
	}

	catalogResponse := httptest.NewRecorder()
	server.ServeHTTP(catalogResponse, newLocalRequest(http.MethodGet, "/api/commands", nil))
	if catalogResponse.Code != http.StatusOK {
		t.Fatalf("catalog status = %d", catalogResponse.Code)
	}
	var catalog apiCatalog
	if err := json.Unmarshal(catalogResponse.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Tabs) != 4 {
		t.Fatalf("tabs = %d", len(catalog.Tabs))
	}
	if catalog.Tabs[0].ID != "tcpnat" || catalog.Tabs[1].ID != "stun" || catalog.Tabs[2].ID != "punch" || catalog.Tabs[3].ID != "birthday" {
		t.Fatalf("unexpected tab order: %+v", catalog.Tabs)
	}
	if len(catalog.Tabs[2].Commands) != 2 || len(catalog.Tabs[3].Commands) != 2 {
		t.Fatal("role-aware tabs must expose client and server")
	}
	for _, tab := range catalog.Tabs {
		for _, command := range tab.Commands {
			if hasAPIField(command, "signal_path") {
				t.Fatalf("command %s exposes separate signal_path", command.ID)
			}
		}
	}
	for _, endpoint := range []string{"/api/punch/client", "/api/punch/server"} {
		punch, ok := catalog.commandByEndpoint(endpoint)
		if !ok {
			t.Fatalf("%s command missing", endpoint)
		}
		for _, removed := range []string{"tcp", "nat", "stun"} {
			if hasAPIField(punch, removed) {
				t.Fatalf("%s exposes removed field %s", endpoint, removed)
			}
		}
	}
	birthdayServer, ok := catalog.commandByEndpoint("/api/birthday/server")
	if !ok {
		t.Fatal("birthday server command missing")
	}
	serverSignal, ok := apiFieldByName(birthdayServer, "signal")
	if !ok || serverSignal.Input != "textarea" || !serverSignal.Required || serverSignal.Label != "Signal servers" {
		t.Fatalf("birthday server signal field = %+v, exists=%v", serverSignal, ok)
	}
	if hasAPIField(birthdayServer, "probe-signals") {
		t.Fatal("birthday server exposes internal probe-signals field")
	}
	serverMode, ok := apiFieldByName(birthdayServer, "mode")
	if !ok || serverMode.Default != "nat" || len(serverMode.Options) != 2 || serverMode.Options[0] != "nat" || serverMode.Options[1] != "fixed" {
		t.Fatalf("birthday server mode field = %+v, exists=%v", serverMode, ok)
	}
	for _, clientOwned := range []string{"candidates", "trials", "min-port", "max-port", "warmup", "attempt-timeout"} {
		if hasAPIField(birthdayServer, clientOwned) {
			t.Fatalf("birthday server exposes client-owned field %s", clientOwned)
		}
	}
}

func hasAPIField(command apiCommandSpec, name string) bool {
	_, exists := apiFieldByName(command, name)
	return exists
}

func apiFieldByName(command apiCommandSpec, name string) (apiFieldSpec, bool) {
	for _, field := range command.Fields {
		if field.Name == name {
			return field, true
		}
	}
	return apiFieldSpec{}, false
}

func TestAPIBirthdayServerUsesFirstSignalNodeForPairing(t *testing.T) {
	primary, probes, err := apiBirthdayServerSignals(map[string]any{
		"signal": "203.0.113.10:9960/signal\n198.51.100.20:9962/edge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if primary != "203.0.113.10:9960/signal" {
		t.Fatalf("primary = %q", primary)
	}
	if probes != "203.0.113.10:9960/signal\n198.51.100.20:9962/edge" {
		t.Fatalf("probes = %q", probes)
	}
}

func TestAPIRejectsInvalidArguments(t *testing.T) {
	server, err := newAPIHTTPServer()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path string
		body string
		want string
	}{
		{path: "/api/punch/client", body: `{"arguments":{"room":"demo"}}`, want: "signal"},
		{path: "/api/tcpnat", body: `{"arguments":{"server":"127.0.0.1","unknown":1}}`, want: "unknown argument"},
		{path: "/api/stun", body: `{"arguments":{"stun":""}}`, want: "must not be empty"},
		{path: "/api/birthday/server", body: `{"arguments":{"signal":"203.0.113.10:9960/signal","mode":"fixed"}}`, want: "local-port"},
		{path: "/api/birthday/server", body: `{"arguments":{"mode":"nat"}}`, want: "argument signal is required"},
		{path: "/api/birthday/server", body: `{"arguments":{"signal":"signal.example:9960/path"}}`, want: "IPv4"},
	}
	for _, test := range tests {
		t.Run(test.path+test.want, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := newLocalRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("body %q missing %q", response.Body.String(), test.want)
			}
		})
	}
}

func TestAPIIntegratedSTUNRun(t *testing.T) {
	server, err := newAPIHTTPServer()
	if err != nil {
		t.Fatal(err)
	}
	stunServer := startBindingOnlySTUNServer(t)
	body := fmt.Sprintf(`{"arguments":{"stun":%q,"timeout":"2s"}}`, stunServer)
	response := httptest.NewRecorder()
	request := newLocalRequest(http.MethodPost, "/api/stun", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body = %s", response.Code, response.Body.String())
	}
	var started apiRunView
	if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	finished := waitForTerminalRun(t, server.runs, started.ID)
	if finished.Status != "succeeded" || !strings.Contains(finished.Log, "public UDP mapping") {
		t.Fatalf("status = %s, log = %s", finished.Status, finished.Log)
	}
}

func TestAPIIntegratedRunAndSSE(t *testing.T) {
	server, err := newAPIHTTPServer()
	if err != nil {
		t.Fatal(err)
	}
	body := `{"arguments":{"server":"127.0.0.1/signal","ports":"1,2","iface":"","rounds":1,"timeout":"1ms"}}`
	startResponse := httptest.NewRecorder()
	request := newLocalRequest(http.MethodPost, "/api/tcpnat", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(startResponse, request)
	if startResponse.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body = %s", startResponse.Code, startResponse.Body.String())
	}
	var started apiRunView
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	run := waitForTerminalRun(t, server.runs, started.ID)
	if run.Status != "failed" {
		t.Fatalf("status = %s, log = %s", run.Status, run.Log)
	}
	if !strings.Contains(run.Log, "[run] tcpnat") || !strings.Contains(run.Log, "[error]") {
		t.Fatalf("unexpected run log: %s", run.Log)
	}

	eventsResponse := httptest.NewRecorder()
	eventsRequest := newLocalRequest(http.MethodGet, "/api/runs/"+started.ID+"/events", nil)
	server.ServeHTTP(eventsResponse, eventsRequest)
	events := eventsResponse.Body.String()
	if !strings.Contains(events, "event: output") || !strings.Contains(events, "event: state") {
		t.Fatalf("unexpected SSE response: %s", events)
	}
}

func TestAPIStopsIntegratedRun(t *testing.T) {
	catalog := newAPICatalog()
	spec, ok := catalog.commandByEndpoint("/api/punch/client")
	if !ok {
		t.Fatal("punch client spec missing")
	}
	values, err := normalizeAPICommandValues(spec, map[string]any{
		"signal": "127.0.0.1:1",
		"room":   "cancel-test",
		"iface":  "",
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := newAPIRunManager()
	started, err := manager.start(spec, values)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.stop(started.ID); err != nil {
		t.Fatal(err)
	}
	finished := waitForTerminalRun(t, manager, started.ID)
	if finished.Status != "canceled" {
		t.Fatalf("status = %s, error = %s, log = %s", finished.Status, finished.Error, finished.Log)
	}
}

func waitForTerminalRun(t *testing.T, manager *apiRunManager, id string) apiRunView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		run, exists := manager.get(id)
		if !exists {
			t.Fatalf("run %s missing", id)
		}
		view := run.snapshot(true)
		if isTerminalRunStatus(view.Status) {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	var buffer bytes.Buffer
	if run, exists := manager.get(id); exists {
		view := run.snapshot(true)
		buffer.WriteString(view.Status)
		buffer.WriteString(": ")
		buffer.WriteString(view.Log)
	}
	t.Fatalf("run did not finish: %s", buffer.String())
	return apiRunView{}
}

func (catalog apiCatalog) commandByEndpoint(endpoint string) (apiCommandSpec, bool) {
	for _, tab := range catalog.Tabs {
		for _, command := range tab.Commands {
			if command.Endpoint == endpoint {
				return command, true
			}
		}
	}
	return apiCommandSpec{}, false
}

// newLocalRequest builds a request addressed to the default loopback listen
// address, as the embedded console would send it.
func newLocalRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Host = defaultAPIListenAddress
	return request
}

func TestAPIRejectsForeignRequests(t *testing.T) {
	server, err := newAPIHTTPServer()
	if err != nil {
		t.Fatal(err)
	}
	const body = `{"arguments":{"stun":""}}`
	tests := []struct {
		name        string
		method      string
		path        string
		host        string
		origin      string
		contentType string
		want        int
	}{
		{name: "rebound host", method: http.MethodGet, path: "/api/runs", host: "attacker.example:18083", want: http.StatusForbidden},
		{name: "rebound host index", method: http.MethodGet, path: "/", host: "attacker.example", want: http.StatusForbidden},
		{name: "empty host", method: http.MethodGet, path: "/api/runs", host: "", want: http.StatusForbidden},
		{name: "lan host", method: http.MethodGet, path: "/api/runs", host: "192.168.1.10:18083", want: http.StatusForbidden},
		{name: "cross origin get", method: http.MethodGet, path: "/api/runs", host: "127.0.0.1:18083", origin: "http://attacker.example", want: http.StatusForbidden},
		{name: "null origin", method: http.MethodPost, path: "/api/stun", host: "127.0.0.1:18083", origin: "null", contentType: "application/json", want: http.StatusForbidden},
		{name: "origin port mismatch", method: http.MethodPost, path: "/api/stun", host: "127.0.0.1:18083", origin: "http://127.0.0.1:9999", contentType: "application/json", want: http.StatusForbidden},
		{name: "text plain post", method: http.MethodPost, path: "/api/stun", host: "127.0.0.1:18083", contentType: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "form post", method: http.MethodPost, path: "/api/stun", host: "localhost:18083", contentType: "application/x-www-form-urlencoded", want: http.StatusUnsupportedMediaType},
		{name: "missing content type", method: http.MethodPost, path: "/api/stun", host: "127.0.0.1:18083", want: http.StatusUnsupportedMediaType},
		{name: "delete without json", method: http.MethodDelete, path: "/api/runs/none", host: "127.0.0.1:18083", want: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(body))
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
	if runs := server.runs.list(); len(runs) != 0 {
		t.Fatalf("rejected requests must not start runs, got %d", len(runs))
	}
}

func TestAPIAcceptsLocalRequests(t *testing.T) {
	server, err := newAPIHTTPServer()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		method string
		path   string
		host   string
		origin string
		want   int
	}{
		{name: "ipv4 loopback", method: http.MethodGet, path: "/api/commands", host: "127.0.0.1:18083", want: http.StatusOK},
		{name: "other loopback", method: http.MethodGet, path: "/", host: "127.0.0.2:8080", want: http.StatusOK},
		{name: "ipv6 loopback", method: http.MethodGet, path: "/api/commands", host: "[::1]:18083", want: http.StatusOK},
		{name: "ipv6 loopback no port", method: http.MethodGet, path: "/api/commands", host: "[::1]", want: http.StatusOK},
		{name: "localhost no port", method: http.MethodGet, path: "/api/commands", host: "localhost", want: http.StatusOK},
		{name: "localhost upper case", method: http.MethodGet, path: "/api/commands", host: "LOCALHOST:18083", want: http.StatusOK},
		{name: "same origin get", method: http.MethodGet, path: "/api/runs", host: "localhost:18083", origin: "http://localhost:18083", want: http.StatusOK},
		// The console's own POST: same origin, JSON body. The arguments are
		// invalid on purpose so no run starts; reaching validation proves
		// the request passed the guard.
		{name: "console post", method: http.MethodPost, path: "/api/stun", host: "127.0.0.1:18083", origin: "http://127.0.0.1:18083", want: http.StatusBadRequest},
		{name: "console delete", method: http.MethodDelete, path: "/api/runs/none", host: "127.0.0.1:18083", origin: "http://127.0.0.1:18083", want: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var body io.Reader
			if test.method == http.MethodPost {
				body = strings.NewReader(`{"arguments":{"stun":""}}`)
			}
			request := httptest.NewRequest(test.method, test.path, body)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.method != http.MethodGet {
				request.Header.Set("Content-Type", "application/json; charset=utf-8")
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestAPIListenAddressMustBeLoopback(t *testing.T) {
	for _, address := range []string{defaultAPIListenAddress, "127.0.0.1:0", "[::1]:18083", "localhost:18083"} {
		if err := checkAPIListenAddress(address); err != nil {
			t.Fatalf("%s should be accepted: %v", address, err)
		}
	}
	for _, address := range []string{":18083", "0.0.0.0:18083", "[::]:18083", "192.168.1.10:18083", "example.com:18083", "127.0.0.1"} {
		if err := checkAPIListenAddress(address); err == nil {
			t.Fatalf("%s should be refused", address)
		}
	}
	// runAPIServer refuses before binding anything.
	if err := runAPIServer(context.Background(), apiConfig{listen: "0.0.0.0:0"}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("runAPIServer: err = %v, want a loopback error", err)
	}
}
