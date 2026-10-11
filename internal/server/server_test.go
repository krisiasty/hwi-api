package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/internal/auth"
	"github.com/krisiasty/hwi-api/internal/config"
)

func TestBuildTLSConfigVersionFloor(t *testing.T) {
	// A test certificate and key are written to a temp dir by the helper
	// added in this step; see writeTestKeyPair below.
	cert, key := writeTestKeyPair(t)

	c, err := BuildTLSConfig(config.TLSConfig{CertFile: cert, KeyFile: key, MinVersion: "1.3"}, nil)
	if err != nil {
		t.Fatalf("BuildTLSConfig: %v", err)
	}
	if c.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %x, want TLS 1.3", c.MinVersion)
	}

	c, err = BuildTLSConfig(config.TLSConfig{CertFile: cert, KeyFile: key, MinVersion: "1.2"}, nil)
	if err != nil {
		t.Fatalf("BuildTLSConfig: %v", err)
	}
	if c.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", c.MinVersion)
	}
	if len(c.CipherSuites) == 0 {
		t.Error("lowering the floor to 1.2 must pin an AEAD-only suite list")
	}
}

func TestBuildTLSConfigClientAuthMode(t *testing.T) {
	cert, key := writeTestKeyPair(t)
	ca := writeTestCA(t)

	mtlsOnly, err := auth.New(config.AuthConfig{MTLS: config.MTLSConfig{Enabled: true, ClientCAFile: ca}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := BuildTLSConfig(config.TLSConfig{CertFile: cert, KeyFile: key, MinVersion: "1.3"}, mtlsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert when mTLS is the only mechanism", c.ClientAuth)
	}
	// A ClientAuth that verifies is worthless without the pool to verify
	// against: if ClientCAs were left nil here, crypto/tls would fall back
	// to the system root pool, and any publicly-trusted certificate - not
	// just one signed by this deployment's configured CA - would produce a
	// non-empty VerifiedChains. Asserting ClientAuth alone would not catch
	// that regression; TestBuildTLSConfigClientAuthMode is exactly the test
	// that must.
	if c.ClientCAs == nil {
		t.Fatal("ClientCAs is nil: verification would fall back to the system root pool")
	}
	if c.ClientCAs != mtlsOnly.ClientCAs() {
		t.Error("ClientCAs is not the authenticator's own pool")
	}

	both, err := auth.New(config.AuthConfig{
		MTLS:  config.MTLSConfig{Enabled: true, ClientCAFile: ca},
		Token: config.TokenConfig{Enabled: true, Hashes: []string{"sha256:" + hex64()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err = BuildTLSConfig(config.TLSConfig{CertFile: cert, KeyFile: key, MinVersion: "1.3"}, both)
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %v, want VerifyClientCertIfGiven so token-only clients can connect", c.ClientAuth)
	}
	if c.ClientCAs == nil {
		t.Fatal("ClientCAs is nil: verification would fall back to the system root pool")
	}
	if c.ClientCAs != both.ClientCAs() {
		t.Error("ClientCAs is not the authenticator's own pool")
	}
}

func TestServeBindsAndShutsDownGracefully(t *testing.T) {
	opts := Options{
		Name:    "test",
		Addrs:   []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		Port:    0,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		Log:     quiet(),
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	select {
	case <-srv.Bound():
	case err := <-done:
		t.Fatalf("Serve returned %v before binding", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Bound was not closed after Serve opened its listener")
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil after a graceful shutdown triggered by ctx", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after ctx was canceled")
	}
}

// TestServeFailsWholeServerOnAnyBindFailure exercises the invariant that a
// bind failure on any one configured address fails Serve entirely, with
// every listener it had already opened rolled back - rather than leaving
// the server quietly running on fewer addresses than configured.
//
// It needs no loopback alias (127.0.0.2 is not usable on every host
// without extra configuration): pre-binding 127.0.0.1:0 ourselves reserves a port that is
// guaranteed free on ::1 but already taken on 127.0.0.1, so a Server
// configured to bind both, in that order, always succeeds on the first
// and always collides on the second.
func TestServeFailsWholeServerOnAnyBindFailure(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-binding a blocking listener: %v", err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	opts := Options{
		Name: "test",
		Addrs: []netip.Addr{
			netip.MustParseAddr("::1"),       // must bind successfully
			netip.MustParseAddr("127.0.0.1"), // must fail: blocker already holds this one
		},
		Port:    port,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		Log:     quiet(),
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve succeeded despite one configured address being unbindable")
	}
	select {
	case <-srv.Bound():
		t.Error("Bound was closed although binding failed")
	default:
	}

	// The ::1 listener that did succeed must have been closed again:
	// binding that exact address and port ourselves must now succeed.
	ln, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Errorf("[::1]:%d is still held after Serve failed: %v; "+
			"a successful listener was not rolled back", port, err)
		return
	}
	ln.Close()
}

// writeTestKeyPair generates a self-signed ECDSA certificate and PEM-encodes
// it and its key to files in t.TempDir(), the same approach
// internal/auth/testca_test.go uses so no fixture files are needed and
// nothing expires.
func writeTestKeyPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-server"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "server.pem")
	keyPath = filepath.Join(dir, "server-key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatal(err)
	}

	return certPath, keyPath
}

// writeTestCA generates a self-signed CA certificate and PEM-encodes it to
// a file in t.TempDir(), suitable as a config.MTLSConfig.ClientCAFile value.
func writeTestCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hex64 returns 64 hex characters (32 arbitrary bytes) shaped like a sha256
// digest, for a token config that must decode as one but is never actually
// presented as a credential in this test.
func hex64() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// The TLS files are read without tls.LoadX509KeyPair, whose read errors
// carry the resolved path: for a credential that path ends in its name,
// which may be a pasted token.
func TestBuildTLSConfigErrorsNeverIncludeACredentialName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	certName := strings.Repeat("3f9a", 16)
	keyName := strings.Repeat("7c21", 16)
	if err := os.WriteFile(filepath.Join(dir, certName), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, keyName), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cert, key, want string
	}{
		{"unreadable key", "credential:" + certName, "credential:" + keyName, "tls.key_file"},
		{"certificate that does not parse", "credential:" + certName, "credential:" + certName, "loading TLS key pair"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildTLSConfig(config.TLSConfig{CertFile: tc.cert, KeyFile: tc.key, MinVersion: "1.3"}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %s", err, tc.want)
			}
			for _, leak := range []string{certName[:8], keyName[:8], dir} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error includes a credential's name or path: %v", err)
				}
			}
		})
	}
}
