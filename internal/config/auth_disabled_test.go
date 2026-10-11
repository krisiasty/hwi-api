package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateAuthDisabledIgnoresInactiveMechanisms: with api.auth.enabled
// false no authenticator is built, so an enabled mechanism whose
// credentials are missing or malformed has no effect and must not stop
// startup.
func TestValidateAuthDisabledIgnoresInactiveMechanisms(t *testing.T) {
	cases := map[string]func(*Config){
		"token enabled with no digest": func(c *Config) {
			c.API.Auth.Token.Enabled = true
			c.API.Auth.Token.Hashes = nil
			c.API.Auth.Token.HashesFile = ""
		},
		"token enabled with a malformed digest": func(c *Config) {
			c.API.Auth.Token.Enabled = true
			c.API.Auth.Token.Hashes = []string{"sha256:not-hex"}
		},
		"mtls enabled with no client CA": func(c *Config) {
			c.API.Auth.MTLS.Enabled = true
			c.API.Auth.MTLS.ClientCAFile = ""
		},
		"mtls enabled with a missing client CA file": func(c *Config) {
			c.API.Auth.MTLS.Enabled = true
			c.API.Auth.MTLS.ClientCAFile = "/nonexistent/client-ca.pem"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			mutate(&cfg)
			cfg.API.Auth.Enabled = false

			warnings, err := cfg.Validate()
			if err != nil {
				t.Fatalf("Validate: %v, want an inactive mechanism to be ignored", err)
			}
			joined := strings.Join(warnings, " ")
			if !strings.Contains(joined, "api.auth.enabled is false") {
				t.Errorf("warnings = %v, want the unauthenticated-access warning kept", warnings)
			}
			if !strings.Contains(joined, "has no effect") {
				t.Errorf("warnings = %v, want one saying the enabled mechanism has no effect", warnings)
			}
		})
	}
}

// TestShippedConfigStartsWithOnlyAuthDisabled is the documented trusted
// proxy setup: take packaging/config.yaml, which enables token auth with
// no digest pending operator setup, and change only api.auth.enabled. The
// TLS certificate and key are supplied, because TLS stays enabled and is
// required independently of authentication.
func TestShippedConfigStartsWithOnlyAuthDisabled(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "packaging", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := Decode(f)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !cfg.API.Auth.Token.Enabled || len(cfg.API.Auth.Token.Hashes) != 0 || cfg.API.Auth.Token.HashesFile != "" {
		t.Fatal("the shipped config no longer enables token auth with no digest; this test's premise needs updating")
	}

	valid := validConfig(t)
	cfg.API.TLS.CertFile = valid.API.TLS.CertFile
	cfg.API.TLS.KeyFile = valid.API.TLS.KeyFile
	cfg.API.Auth.Enabled = false

	if _, err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v, want the shipped config to start with only api.auth.enabled set to false", err)
	}
}

// TestValidateAuthEnabledStillRequiresMechanismCredentials guards the other
// side: with auth on, the existing requirements are unchanged.
func TestValidateAuthEnabledStillRequiresMechanismCredentials(t *testing.T) {
	cfg := validConfig(t)
	cfg.API.Auth.Token.Hashes = nil
	if _, err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "no digest is configured") {
		t.Errorf("Validate: %v, want the missing-digest error while auth is enabled", err)
	}

	cfg = validConfig(t)
	cfg.API.Auth.MTLS.Enabled = true
	if _, err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "client_ca_file is required") {
		t.Errorf("Validate: %v, want the missing-CA error while auth is enabled", err)
	}
}
