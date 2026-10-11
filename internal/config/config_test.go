package config

import (
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

const sample = `
api:
  bind:
    addresses: ["10.0.0.5"]
    interfaces: ["eth0"]
    port: 8443
  tls:
    cert_file: credential:tls_cert.pem
    key_file: credential:tls_key.pem
    min_version: "1.2"
  auth:
    mtls:
      enabled: true
      client_ca_file: /etc/hwi-api/client-ca.pem
      allowed_subjects: ["CN=inventory.example.com"]
    token:
      enabled: true
      hashes: ["sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"]
  access:
    allow: ["10.0.0.0/16"]
    deny: ["10.0.7.13/32"]
  timeouts:
    read: 25s
  max_concurrent_requests: 64
inventory:
  refresh:
    interval: 2m
    subsystems:
      pci: { interval: 30m }
      udev-drm: { enabled: false }
log:
  level: debug
`

func TestDecodeReadsEverySection(t *testing.T) {
	cfg, err := Decode(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if got := cfg.API.Bind.Addresses; len(got) != 1 || got[0] != "10.0.0.5" {
		t.Errorf("api.bind.addresses = %v", got)
	}
	if got := cfg.API.Bind.Interfaces; len(got) != 1 || got[0] != "eth0" {
		t.Errorf("api.bind.interfaces = %v", got)
	}
	if cfg.API.TLS.MinVersion != "1.2" {
		t.Errorf("api.tls.min_version = %q", cfg.API.TLS.MinVersion)
	}
	if !cfg.API.Auth.MTLS.Enabled || !cfg.API.Auth.Token.Enabled {
		t.Error("both auth mechanisms should have decoded as enabled")
	}
	if cfg.API.MaxConcurrentRequests != 64 {
		t.Errorf("api.max_concurrent_requests = %d", cfg.API.MaxConcurrentRequests)
	}
	if cfg.API.Timeouts.Read.Duration() != 25*time.Second {
		t.Errorf("api.timeouts.read = %s", cfg.API.Timeouts.Read.Duration())
	}
	if cfg.Inventory.Refresh.Interval.Duration() != 2*time.Minute {
		t.Errorf("inventory.refresh.interval = %s", cfg.Inventory.Refresh.Interval.Duration())
	}
	if got := cfg.Inventory.Refresh.Subsystems[apiv1.SubsystemPCI].Interval.Duration(); got != 30*time.Minute {
		t.Errorf("pci interval = %s", got)
	}
	drm := cfg.Inventory.Refresh.Subsystems[apiv1.SubsystemUdevDRM]
	if drm.Enabled == nil || *drm.Enabled {
		t.Errorf("udev-drm.enabled = %v, want an explicit false", drm.Enabled)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q", cfg.Log.Level)
	}
}

func TestDecodeAppliesDefaultsForOmittedSettings(t *testing.T) {
	cfg, err := Decode(strings.NewReader("api:\n  bind:\n    port: 9443\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if cfg.API.Bind.Port != 9443 {
		t.Errorf("explicit port was overwritten: %d", cfg.API.Bind.Port)
	}
	if cfg.API.TLS.MinVersion != "1.3" {
		t.Errorf("default min_version = %q, want 1.3", cfg.API.TLS.MinVersion)
	}
	if !cfg.API.TLS.Enabled {
		t.Error("api.tls.enabled should default to true when the key is absent")
	}
	if !cfg.API.Auth.Enabled {
		t.Error("api.auth.enabled should default to true when the key is absent")
	}
	if cfg.API.Timeouts.ReadHeader.Duration() != 5*time.Second {
		t.Errorf("default read_header = %s", cfg.API.Timeouts.ReadHeader.Duration())
	}
	if cfg.API.MaxConcurrentRequests != 32 {
		t.Errorf("default max_concurrent_requests = %d", cfg.API.MaxConcurrentRequests)
	}
	// This input sets the api port, so check its default directly.
	if got := Defaults().API.Bind.Port; got != 8096 {
		t.Errorf("default api port = %d", got)
	}
	if cfg.Ops.Bind.Port != 9096 {
		t.Errorf("default ops port = %d", cfg.Ops.Bind.Port)
	}
	if got := cfg.Ops.Bind.Addresses; len(got) != 2 || got[0] != "127.0.0.1" || got[1] != "::1" {
		t.Errorf("default ops addresses = %v, want loopback only", got)
	}
	if got := cfg.Ops.Access.Allow; len(got) != 2 {
		t.Errorf("default ops allow list = %v, want loopback prefixes", got)
	}
	if cfg.Inventory.Refresh.Interval.Duration() != time.Minute {
		t.Errorf("default refresh interval = %s", cfg.Inventory.Refresh.Interval.Duration())
	}
	if cfg.Log.Level != "info" {
		t.Errorf("default log level = %q", cfg.Log.Level)
	}
}

func TestDecodeEmptyListReplacesDefault(t *testing.T) {
	cfg, err := Decode(strings.NewReader("ops:\n  access:\n    allow: []\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(cfg.Ops.Access.Allow) != 0 {
		t.Errorf("ops.access.allow = %v, want empty: an explicit empty list means the operator meant empty", cfg.Ops.Access.Allow)
	}
}

func TestDecodeEmptyFileYieldsDefaults(t *testing.T) {
	cfg, err := Decode(strings.NewReader(""))
	if err != nil {
		t.Fatalf("an empty configuration file is valid: %v", err)
	}
	if cfg.API.Bind.Port != Defaults().API.Bind.Port || cfg.Log.Level != Defaults().Log.Level {
		t.Errorf("empty file produced %+v, want the defaults", cfg)
	}
}

func TestDecodeAppliesDefaultsForOmittedSiblingSettings(t *testing.T) {
	cfg, err := Decode(strings.NewReader("api:\n  timeouts:\n    read: 25s\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	want := Defaults()
	if cfg.API.Timeouts.Read.Duration() != 25*time.Second {
		t.Errorf("api.timeouts.read = %s, want 25s", cfg.API.Timeouts.Read.Duration())
	}
	if cfg.API.Timeouts.ReadHeader != want.API.Timeouts.ReadHeader {
		t.Errorf("api.timeouts.read_header = %s, want default %s", cfg.API.Timeouts.ReadHeader.Duration(), want.API.Timeouts.ReadHeader.Duration())
	}
	if cfg.API.Timeouts.Write != want.API.Timeouts.Write {
		t.Errorf("api.timeouts.write = %s, want default %s", cfg.API.Timeouts.Write.Duration(), want.API.Timeouts.Write.Duration())
	}
	if cfg.API.Timeouts.Idle != want.API.Timeouts.Idle {
		t.Errorf("api.timeouts.idle = %s, want default %s", cfg.API.Timeouts.Idle.Duration(), want.API.Timeouts.Idle.Duration())
	}
}

func TestDecodeRejectsUnknownKeys(t *testing.T) {
	_, err := Decode(strings.NewReader("api:\n  bnid:\n    port: 8443\n"))
	if err == nil {
		t.Fatal("a misspelled key must be an error")
	}
	if !strings.Contains(err.Error(), "bnid") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestDecodeRejectsUnknownSubsystem(t *testing.T) {
	_, err := Decode(strings.NewReader("inventory:\n  refresh:\n    subsystems:\n      firmware: { interval: 5m }\n"))
	if err == nil {
		t.Fatal("an unknown subsystem name must be an error")
	}
	if !strings.Contains(err.Error(), "firmware") {
		t.Errorf("error should name the unknown subsystem, got: %v", err)
	}
}

func TestDurationRejectsBadValues(t *testing.T) {
	for _, in := range []string{"inventory:\n  refresh:\n    interval: soon\n", "inventory:\n  refresh:\n    interval: 5\n"} {
		if _, err := Decode(strings.NewReader(in)); err == nil {
			t.Errorf("Decode(%q) succeeded, want an error", in)
		}
	}
}

func TestDecodeTLSEnabledCanBeSetFalse(t *testing.T) {
	cfg, err := Decode(strings.NewReader("api:\n  tls:\n    enabled: false\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.API.TLS.Enabled {
		t.Error("api.tls.enabled: false should decode as false, not fall back to the true default")
	}
}

func TestDecodeAuthEnabledCanBeSetFalse(t *testing.T) {
	cfg, err := Decode(strings.NewReader("api:\n  auth:\n    enabled: false\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.API.Auth.Enabled {
		t.Error("api.auth.enabled: false should decode as false, not fall back to the true default")
	}
}

func TestDecodeAcceptsJSON(t *testing.T) {
	// YAML 1.2 is a JSON superset, so an operator may write JSON instead.
	cfg, err := Decode(strings.NewReader(`{"api": {"bind": {"port": 7443}}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cfg.API.Bind.Port != 7443 {
		t.Errorf("port = %d, want 7443", cfg.API.Bind.Port)
	}
}
