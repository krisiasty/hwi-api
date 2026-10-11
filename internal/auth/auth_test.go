package auth

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krisiasty/hwi-api/internal/config"
)

const goodToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func tokenOnly() config.AuthConfig {
	return config.AuthConfig{Token: config.TokenConfig{Enabled: true, Hashes: []string{digestOf(goodToken)}}}
}

func request(t *testing.T, header string, chain []*x509.Certificate) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/inventory/cpu", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	if chain != nil {
		r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{chain}}
	}
	return r
}

func TestTokenAuthentication(t *testing.T) {
	a, err := New(tokenOnly())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if m, err := a.Authenticate(request(t, "Bearer "+goodToken, nil)); err != nil || m != MethodToken {
		t.Errorf("valid token: method %q, err %v", m, err)
	}
	if _, err := a.Authenticate(request(t, "Bearer wrong", nil)); !errors.Is(err, ErrRejected) {
		t.Errorf("wrong token: err = %v, want ErrRejected", err)
	}
	if _, err := a.Authenticate(request(t, "", nil)); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no credentials: err = %v, want ErrNoCredentials", err)
	}
	if _, err := a.Authenticate(request(t, "Basic abc", nil)); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("non-bearer scheme: err = %v, want ErrNoCredentials", err)
	}
}

func TestMTLSAuthentication(t *testing.T) {
	ca := newTestCA(t)
	cfg := config.AuthConfig{MTLS: config.MTLSConfig{Enabled: true, ClientCAFile: ca.writePEM(t)}}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	leaf := ca.issue(t, "inventory.example.com")
	if m, err := a.Authenticate(request(t, "", []*x509.Certificate{leaf, ca.cert})); err != nil || m != MethodMTLS {
		t.Errorf("verified chain: method %q, err %v", m, err)
	}
	if _, err := a.Authenticate(request(t, "", nil)); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no certificate: err = %v, want ErrNoCredentials", err)
	}
}

func TestMTLSSubjectPinning(t *testing.T) {
	ca := newTestCA(t)
	caFile := ca.writePEM(t)

	// A DN-shaped pin ("CN=...") narrows to that one Subject identity: a
	// SAN must not satisfy it, even from a certificate the same CA vouched
	// for.
	cfg := config.AuthConfig{MTLS: config.MTLSConfig{
		Enabled:         true,
		ClientCAFile:    caFile,
		AllowedSubjects: []string{"CN=inventory.example.com"},
	}}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	allowed := ca.issue(t, "inventory.example.com")
	if _, err := a.Authenticate(request(t, "", []*x509.Certificate{allowed, ca.cert})); err != nil {
		t.Errorf("pinned subject was rejected: %v", err)
	}

	other := ca.issue(t, "laptop.example.com")
	if _, err := a.Authenticate(request(t, "", []*x509.Certificate{other, ca.cert})); !errors.Is(err, ErrRejected) {
		t.Errorf("unpinned subject: err = %v, want ErrRejected", err)
	}

	bySAN := ca.issue(t, "someone", "inventory.example.com")
	if _, err := a.Authenticate(request(t, "", []*x509.Certificate{bySAN, ca.cert})); !errors.Is(err, ErrRejected) {
		t.Errorf("a DN-shaped pin should not be satisfied by a SAN match: err = %v, want ErrRejected", err)
	}

	// A bare pin (no "=") is a name, not a DN: it is satisfied by a CN or
	// a SAN match. Same certificate as above, different authenticator.
	bareCfg := config.AuthConfig{MTLS: config.MTLSConfig{
		Enabled:         true,
		ClientCAFile:    caFile,
		AllowedSubjects: []string{"inventory.example.com"},
	}}
	bareAuth, err := New(bareCfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := bareAuth.Authenticate(request(t, "", []*x509.Certificate{bySAN, ca.cert})); err != nil {
		t.Errorf("a bare pin should be satisfied by a SAN match: %v", err)
	}
}

// TestMTLSFullDNPinIsNotACommonNamePin covers the line between the two
// DN-shaped forms: only a pin naming a single CN may fall back to the
// Subject common name. A full-DN pin must match the whole Subject DN, so a
// CA-issued CN that merely spells out the pin's extra attributes is refused.
func TestMTLSFullDNPinIsNotACommonNamePin(t *testing.T) {
	ca := newTestCA(t)
	caFile := ca.writePEM(t)

	literalCN := ca.issue(t, "inventory-client,O=Trusted")
	literalPlusCN := ca.issue(t, "inventory-client+O=Trusted")
	fullDN := ca.issueSubject(t, pkix.Name{CommonName: "inventory-client", Organization: []string{"Trusted"}})
	extraAttrs := ca.issueSubject(t, pkix.Name{CommonName: "inventory-client", Organization: []string{"Other"}})

	tests := []struct {
		name  string
		pin   string
		leaf  *x509.Certificate
		admit bool
	}{
		{"full-DN pin admits the matching Subject DN", "CN=inventory-client,O=Trusted", fullDN, true},
		{"full-DN pin refuses a CN spelling out its attributes", "CN=inventory-client,O=Trusted", literalCN, false},
		{"full-DN pin refuses a different organization", "CN=inventory-client,O=Trusted", extraAttrs, false},
		{"multi-valued RDN pin refuses a CN spelling out its attributes", "CN=inventory-client+O=Trusted", literalPlusCN, false},
		{"single-CN pin admits a Subject with further attributes", "CN=inventory-client", extraAttrs, true},
		{"escaped single-CN pin admits the literal CN", `CN=inventory-client\,O=Trusted`, literalCN, true},
		{"hex-escaped single-CN pin admits the literal CN", `CN=inventory-client\2CO=Trusted`, literalCN, true},
		{"escaped single-CN pin refuses the unescaped DN", `CN=inventory-client\,O=Trusted`, fullDN, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := New(config.AuthConfig{MTLS: config.MTLSConfig{
				Enabled:         true,
				ClientCAFile:    caFile,
				AllowedSubjects: []string{tt.pin},
			}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = a.Authenticate(request(t, "", []*x509.Certificate{tt.leaf, ca.cert}))
			if tt.admit && err != nil {
				t.Errorf("pin %q, subject %q: err = %v, want admitted", tt.pin, tt.leaf.Subject, err)
			}
			if !tt.admit && !errors.Is(err, ErrRejected) {
				t.Errorf("pin %q, subject %q: err = %v, want ErrRejected", tt.pin, tt.leaf.Subject, err)
			}
		})
	}
}

func TestMTLSEnabledAndTokenEnabledReflectConfig(t *testing.T) {
	ca := newTestCA(t)

	tokenOnlyAuth, err := New(tokenOnly())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tokenOnlyAuth.MTLSEnabled() {
		t.Error("token-only config: MTLSEnabled() = true, want false")
	}
	if !tokenOnlyAuth.TokenEnabled() {
		t.Error("token-only config: TokenEnabled() = false, want true")
	}

	mtlsOnlyAuth, err := New(config.AuthConfig{MTLS: config.MTLSConfig{Enabled: true, ClientCAFile: ca.writePEM(t)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !mtlsOnlyAuth.MTLSEnabled() {
		t.Error("mTLS-only config: MTLSEnabled() = false, want true")
	}
	if mtlsOnlyAuth.TokenEnabled() {
		t.Error("mTLS-only config: TokenEnabled() = true, want false")
	}

	bothCfg := tokenOnly()
	bothCfg.MTLS = config.MTLSConfig{Enabled: true, ClientCAFile: ca.writePEM(t)}
	bothAuth, err := New(bothCfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !bothAuth.MTLSEnabled() {
		t.Error("both enabled: MTLSEnabled() = false, want true")
	}
	if !bothAuth.TokenEnabled() {
		t.Error("both enabled: TokenEnabled() = false, want true")
	}
}

func TestEitherMechanismAdmits(t *testing.T) {
	ca := newTestCA(t)
	cfg := tokenOnly()
	cfg.MTLS = config.MTLSConfig{Enabled: true, ClientCAFile: ca.writePEM(t)}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Token alone.
	if m, err := a.Authenticate(request(t, "Bearer "+goodToken, nil)); err != nil || m != MethodToken {
		t.Errorf("token alone: method %q, err %v", m, err)
	}
	// Certificate alone.
	leaf := ca.issue(t, "inventory.example.com")
	if m, err := a.Authenticate(request(t, "", []*x509.Certificate{leaf, ca.cert})); err != nil || m != MethodMTLS {
		t.Errorf("certificate alone: method %q, err %v", m, err)
	}
	// A bad token with a good certificate still passes: either suffices.
	if _, err := a.Authenticate(request(t, "Bearer nope", []*x509.Certificate{leaf, ca.cert})); err != nil {
		t.Errorf("certificate should admit despite a bad token: %v", err)
	}
}

// writeDigestsFile writes content to a fresh temp file and returns its path.
func writeDigestsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "digests")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTokenDigestsFromFile(t *testing.T) {
	// Blank lines and "#" comments alongside a valid entry: the valid one
	// loads, everything else is ignored.
	path := writeDigestsFile(t, "# a comment\n\n"+digestOf(goodToken)+"\n   \n# trailing\n")

	cfg := config.AuthConfig{Token: config.TokenConfig{Enabled: true, HashesFile: path}}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The file path is wired, not just parsed: a token matching the
	// loaded digest actually authenticates.
	if m, err := a.Authenticate(request(t, "Bearer "+goodToken, nil)); err != nil || m != MethodToken {
		t.Errorf("token loaded from hashes_file: method %q, err %v", m, err)
	}
	if _, err := a.Authenticate(request(t, "Bearer wrong", nil)); !errors.Is(err, ErrRejected) {
		t.Errorf("wrong token: err = %v, want ErrRejected", err)
	}
}

func TestTokenDigestsFileOnlyBlanksAndComments(t *testing.T) {
	// Nothing but blanks and comments must not construct an authenticator
	// with an empty, silently-fail-open accept-list.
	path := writeDigestsFile(t, "# nothing to see here\n\n   \n# still nothing\n")

	cfg := config.AuthConfig{Token: config.TokenConfig{Enabled: true, HashesFile: path}}
	if _, err := New(cfg); err == nil {
		t.Error("New: want an error when the hashes file yields no digests, got nil")
	}
}

func TestTokenDigestsFileMalformedEntry(t *testing.T) {
	cases := []struct {
		name  string
		entry string
	}{
		{"wrong length", "sha256:abcd"},
		{"non-hex", "sha256:" + strings.Repeat("z", 64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeDigestsFile(t, c.entry+"\n")
			cfg := config.AuthConfig{Token: config.TokenConfig{Enabled: true, HashesFile: path}}
			if _, err := New(cfg); err == nil {
				t.Error("New: want an error for a malformed digest, got nil")
			}
		})
	}
}

// A credential name can be a pasted token that passes the name rule, so no
// error about a credential's file may include its name, which the resolved
// path ends in: not when the file is unreadable, and not when it resolves
// but holds bad data.
func TestCredentialErrorsNeverIncludeTheName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	name := strings.Repeat("3f9a", 16) // a 64-character hex token, a valid credential name
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
		cfg   config.AuthConfig
		want  string
	}{
		{"client CA with no certificates", writeFile("not a certificate"),
			config.AuthConfig{MTLS: config.MTLSConfig{Enabled: true, ClientCAFile: "credential:" + name}}, "mtls.client_ca_file"},
		{"unreadable client CA", makeDir,
			config.AuthConfig{MTLS: config.MTLSConfig{Enabled: true, ClientCAFile: "credential:" + name}}, "mtls.client_ca_file"},
		{"unreadable hashes file", makeDir,
			config.AuthConfig{Token: config.TokenConfig{Enabled: true, HashesFile: "credential:" + name}}, "token.hashes_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			_ = os.RemoveAll(path)
			tc.setup(t, path)
			_, err := New(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %s", err, tc.want)
			}
			if strings.Contains(err.Error(), name[:8]) || strings.Contains(err.Error(), dir) {
				t.Errorf("error includes the credential's name or path: %v", err)
			}
		})
	}
}

func writeFile(content string) func(*testing.T, string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func makeDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
