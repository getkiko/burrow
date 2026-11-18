package tunnel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkiko/burrow"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

const validClientJSON = `{
  "listen": "127.0.0.1:1080",
  "signal": "ws://example.com:9960/path",
  "room": "test-room",
  "bind_interface": "lo0",
  "strategy": "birthday",
  "birthday": { "selection": "ack", "candidates": 128 }
}`

const validServerJSON = `{
  "forward": "127.0.0.1:29999",
  "signal": "ws://example.com:9960/path",
  "room": "test-room",
  "bind_interface": "lo0",
  "strategy": "birthday",
  "stable_side": true
}`

func TestDurationUnmarshal(t *testing.T) {
	var v struct {
		D Duration `json:"d"`
	}
	if err := json.Unmarshal([]byte(`{"d":"250ms"}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if time.Duration(v.D) != 250*time.Millisecond {
		t.Errorf("D = %v, want 250ms", time.Duration(v.D))
	}
}

func TestDurationUnmarshalInvalid(t *testing.T) {
	var v struct {
		D Duration `json:"d"`
	}
	err := json.Unmarshal([]byte(`{"d":"abc"}`), &v)
	if err == nil {
		t.Fatal("expected unmarshal to fail, but it succeeded")
	}
}

// An unset or zero trials falls back to the default of 100.
func TestLoadClientConfigTrialsDefault(t *testing.T) {
	cfg, err := LoadClientConfig(writeTempConfig(t, validClientJSON))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Trials != defaultTrials {
		t.Errorf("Trials = %d, want default %d", cfg.Trials, defaultTrials)
	}
}

func TestLoadClientConfigTrialsExplicit(t *testing.T) {
	content := `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"direct","trials":5}`
	cfg, err := LoadClientConfig(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Trials != 5 {
		t.Errorf("Trials = %d, want 5", cfg.Trials)
	}
}

func TestLoadClientConfigTrialsNegative(t *testing.T) {
	content := `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"direct","trials":-1}`
	if _, err := LoadClientConfig(writeTempConfig(t, content)); err == nil {
		t.Fatal("expected an error for negative trials")
	}
}

func TestLoadClientConfigValid(t *testing.T) {
	cfg, err := LoadClientConfig(writeTempConfig(t, validClientJSON))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:1080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Room != "test-room" {
		t.Errorf("Room = %q", cfg.Room)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want default info", cfg.LogLevel)
	}
}

func TestLoadClientConfigMissingRequired(t *testing.T) {
	cases := map[string]string{
		"listen":         `{"signal":"ws://a:1/p","room":"r","bind_interface":"lo0","strategy":"direct"}`,
		"signal":         `{"listen":"127.0.0.1:1080","room":"r","bind_interface":"lo0","strategy":"direct"}`,
		"room":           `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","bind_interface":"lo0","strategy":"direct"}`,
		"bind_interface": `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r","strategy":"direct"}`,
		"strategy":       `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r","bind_interface":"lo0"}`,
	}
	for field, content := range cases {
		t.Run(field, func(t *testing.T) {
			_, err := LoadClientConfig(writeTempConfig(t, content))
			if err == nil {
				t.Fatalf("expected an error when %s is missing", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not mention field %q", err.Error(), field)
			}
		})
	}
}

func TestLoadClientConfigRejectsAutoStrategy(t *testing.T) {
	content := `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"auto"}`
	_, err := LoadClientConfig(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("expected an error for strategy=auto")
	}
	if !strings.Contains(err.Error(), "auto") {
		t.Errorf("error %q does not mention auto", err.Error())
	}
}

func TestLoadClientConfigBirthdayRequiresSelection(t *testing.T) {
	content := `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"birthday"}`
	_, err := LoadClientConfig(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("expected an error for birthday without selection")
	}
	if !strings.Contains(err.Error(), "selection") {
		t.Errorf("error %q does not mention selection", err.Error())
	}
}

func TestLoadClientConfigBirthdaySelectionInvalid(t *testing.T) {
	content := `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"birthday",
	             "birthday":{"selection":"bogus"}}`
	_, err := LoadClientConfig(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("expected an error for an invalid selection")
	}
}

func TestLoadServerConfigValid(t *testing.T) {
	cfg, err := LoadServerConfig(writeTempConfig(t, validServerJSON))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Forward != "127.0.0.1:29999" {
		t.Errorf("Forward = %q", cfg.Forward)
	}
	if !cfg.StableSide {
		t.Error("StableSide should be true")
	}
}

// Negotiated parameters in a server config must be rejected. The SDK's
// normalizeListener would reject them anyway; we fail earlier with a clearer
// message.
func TestLoadServerConfigRejectsBirthday(t *testing.T) {
	content := `{"forward":"127.0.0.1:29999","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"birthday","stable_side":true,
	             "birthday":{"selection":"ack"}}`
	_, err := LoadServerConfig(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("expected an error for birthday in a server config")
	}
	if !strings.Contains(err.Error(), "client") {
		t.Errorf("error %q should say the parameter belongs in the client config", err.Error())
	}
}

func TestLoadServerConfigRejectsPortScanRange(t *testing.T) {
	content := `{"forward":"127.0.0.1:29999","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"predictive",
	             "predictive":{"port_scan_range":512}}`
	_, err := LoadServerConfig(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("expected an error for port_scan_range in a server config")
	}
	if !strings.Contains(err.Error(), "client") {
		t.Errorf("error %q should say the parameter belongs in the client config", err.Error())
	}
}

// The server may set the local predictive fields.
func TestLoadServerConfigAllowsLocalPredictive(t *testing.T) {
	content := `{"forward":"127.0.0.1:29999","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"predictive",
	             "predictive":{"concurrency":64,"attempt_timeout":"5s"}}`
	cfg, err := LoadServerConfig(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Predictive.Concurrency != 64 {
		t.Errorf("Concurrency = %d", cfg.Predictive.Concurrency)
	}
	if time.Duration(cfg.Predictive.AttemptTimeout) != 5*time.Second {
		t.Errorf("AttemptTimeout = %v", time.Duration(cfg.Predictive.AttemptTimeout))
	}
}

func TestLoadServerConfigBirthdayRequiresStableSide(t *testing.T) {
	content := `{"forward":"127.0.0.1:29999","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"birthday"}`
	_, err := LoadServerConfig(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("expected an error for birthday with stable_side=false")
	}
	if !strings.Contains(err.Error(), "stable_side") {
		t.Errorf("error %q does not mention stable_side", err.Error())
	}
}

func TestClientConfigToBurrowConfig(t *testing.T) {
	cfg, err := LoadClientConfig(writeTempConfig(t, validClientJSON))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	burrowCfg := cfg.ToBurrowConfig()
	if burrowCfg.SignalAddr != "ws://example.com:9960/path" {
		t.Errorf("SignalAddr = %q", burrowCfg.SignalAddr)
	}
	if burrowCfg.Room != "test-room" {
		t.Errorf("Room = %q", burrowCfg.Room)
	}
	if burrowCfg.BindInterface != "lo0" {
		t.Errorf("BindInterface = %q", burrowCfg.BindInterface)
	}
	if burrowCfg.Strategy != burrow.StrategyBirthday {
		t.Errorf("Strategy = %v", burrowCfg.Strategy)
	}
	if burrowCfg.Birthday.Candidates != 128 {
		t.Errorf("Birthday.Candidates = %d", burrowCfg.Birthday.Candidates)
	}
	if burrowCfg.Birthday.Selection != burrow.CandidateSelectionACK {
		t.Errorf("Birthday.Selection = %v", burrowCfg.Birthday.Selection)
	}
}

// The server-side Config must carry no negotiated fields, or burrow.Listen
// rejects it.
func TestServerConfigToBurrowConfigOmitsNegotiated(t *testing.T) {
	cfg, err := LoadServerConfig(writeTempConfig(t, validServerJSON))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	burrowCfg := cfg.ToBurrowConfig()
	if burrowCfg.Birthday != (burrow.BirthdayOptions{}) {
		t.Errorf("Birthday should be zero, got %+v", burrowCfg.Birthday)
	}
	if burrowCfg.Predictive.PortScanRange != 0 {
		t.Errorf("Predictive.PortScanRange = %d, want 0", burrowCfg.Predictive.PortScanRange)
	}
	if burrowCfg.PortScanRange != 0 {
		t.Errorf("PortScanRange = %d, want 0", burrowCfg.PortScanRange)
	}
	if !burrowCfg.StableSide {
		t.Error("StableSide should be true")
	}
}

func TestLoadClientConfigHosts(t *testing.T) {
	content := `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"direct",
	             "hosts":{"a.com":"1.2.3.4"}}`
	cfg, err := LoadClientConfig(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Hosts["a.com"] != "1.2.3.4" {
		t.Errorf("Hosts = %v", cfg.Hosts)
	}
}

func TestLoadConfigFileNotFound(t *testing.T) {
	_, err := LoadClientConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestLoadConfigMalformedJSON(t *testing.T) {
	_, err := LoadClientConfig(writeTempConfig(t, `{not json`))
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestLogLevelValidation(t *testing.T) {
	content := `{"listen":"127.0.0.1:1080","signal":"ws://a:1/p","room":"r",
	             "bind_interface":"lo0","strategy":"direct","log_level":"bogus"}`
	_, err := LoadClientConfig(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("expected an error for an invalid log_level")
	}
}
