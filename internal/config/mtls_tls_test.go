package config

import (
	"strings"
	"testing"
)

// TestValidateRejectsMTLSOnlyWithoutTLS: with TLS off at this
// listener no request carries a client certificate, so an mTLS-only
// configuration would start and then admit no client at all.
func TestValidateRejectsMTLSOnlyWithoutTLS(t *testing.T) {
	cfg := validConfig(t)
	cfg.API.TLS.Enabled = false
	cfg.API.Auth.Token.Enabled = false
	cfg.API.Auth.MTLS.Enabled = true
	cfg.API.Auth.MTLS.ClientCAFile = cfg.API.TLS.CertFile // any readable file

	_, err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted mTLS as the only mechanism with api.tls.enabled false")
	}
	for _, want := range []string{"api.auth.mtls", "api.tls.enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// With token authentication also enabled, the listener still works, so
// the combination is accepted, with a warning that mTLS is inert. As for
// any inactive mechanism, its CA file is then not required.
func TestValidateWarnsThatMTLSIsInertWithoutTLS(t *testing.T) {
	cfg := validConfig(t)
	cfg.API.TLS.Enabled = false
	cfg.API.Auth.MTLS.Enabled = true
	cfg.API.Auth.MTLS.ClientCAFile = "/nonexistent/client-ca.pem"

	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v, want mTLS plus token without TLS to be accepted", err)
	}
	if !strings.Contains(strings.Join(warnings, " "), "api.auth.mtls.enabled is true but has no effect because api.tls.enabled is false") {
		t.Errorf("warnings = %v, want one saying mTLS has no effect without TLS", warnings)
	}
}

// The existing mTLS requirements still hold whenever TLS is on.
func TestValidateStillRequiresTheCAWithTLS(t *testing.T) {
	cfg := validConfig(t)
	cfg.API.Auth.MTLS.Enabled = true
	cfg.API.Auth.MTLS.ClientCAFile = ""
	if _, err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "client_ca_file is required") {
		t.Errorf("Validate: %v, want the missing-CA error with TLS enabled", err)
	}
}
