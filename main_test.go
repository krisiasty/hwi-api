package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/buildinfo"
	"github.com/krisiasty/hwi-api/internal/helper"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

func TestHostIDIsDerivedAndStable(t *testing.T) {
	fsys := fstest.MapFS{"etc/machine-id": {Data: []byte("3f9a1c2e4b5d6789abcdef0123456789\n")}}

	first := hostID(fsys)
	if first == "" {
		t.Fatal("hostID returned nothing")
	}
	if len(first) != 32 {
		t.Errorf("hostID = %q, want 32 hex characters (128 bits)", first)
	}
	if strings.Contains(first, "3f9a1c2e") {
		t.Error("the raw machine ID must not appear in the derived identifier")
	}
	if again := hostID(fsys); again != first {
		t.Errorf("hostID is not stable: %q then %q", first, again)
	}

	other := hostID(fstest.MapFS{"etc/machine-id": {Data: []byte("00000000000000000000000000000001\n")}})
	if other == first {
		t.Error("different machine IDs must produce different identifiers")
	}
}

// TestHostIDIsAKnownAnswer pins the whole derivation - appUUID, the HMAC
// construction, the whitespace trim and the 128-bit truncation - to one
// fixed output. The other hostID tests check length, stability,
// non-disclosure and that different machine IDs differ, all of which stay
// green if appUUID is edited; only this test notices.
//
// The expected value was independently reproduced with
//
//	printf '3f9a1c2e4b5d6789abcdef0123456789' | openssl dgst -sha256 \
//	  -mac HMAC -macopt hexkey:2f1c9a6d4b8347e29d506a71c83eb415 -r
//
// truncated to the first 32 hex characters, so this is not merely the
// current code agreeing with itself.
func TestHostIDIsAKnownAnswer(t *testing.T) {
	const (
		machineID = "3f9a1c2e4b5d6789abcdef0123456789\n"
		want      = "b10cc3e5d1df86119f19ba5981ddc4d6"
	)

	got := hostID(fstest.MapFS{"etc/machine-id": {Data: []byte(machineID)}})
	if got != want {
		t.Errorf("hostID = %q, want %q.\n"+
			"The host-identifier derivation has changed, and with it every host's "+
			"reported identity: appUUID, the HMAC construction, the whitespace trim "+
			"or the 128-bit truncation is no longer what it was. This is a wire "+
			"contract change, not a test that needs updating - consumers that "+
			"correlate hosts by host.id will see every host as a brand-new "+
			"machine after an upgrade. Restore the derivation, or change this "+
			"value only as a deliberate, documented break.", got, want)
	}
}

func TestHostIDFallsBackToDBus(t *testing.T) {
	fsys := fstest.MapFS{"var/lib/dbus/machine-id": {Data: []byte("abc\n")}}
	if hostID(fsys) == "" {
		t.Error("the dbus location should be used when /etc/machine-id is absent")
	}
}

func TestHostIDIsEmptyWhenUnavailable(t *testing.T) {
	if got := hostID(fstest.MapFS{}); got != "" {
		t.Errorf("hostID = %q, want empty when no machine ID exists", got)
	}
}

func TestRunRejectsAMissingConfig(t *testing.T) {
	err := run(context.Background(),
		[]string{"--config", filepath.Join(t.TempDir(), "absent.yaml")}, io.Discard)
	if err == nil {
		t.Fatal("a missing configuration file must fail the start")
	}
}

func TestRunVersionReportsBuildInfo(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &buf); err != nil {
		t.Fatalf("run(--version) = %v, want nil", err)
	}

	// Compared against what buildinfo actually reports, not hardcoded
	// strings, so this does not drift when ldflags stamp a real
	// version/commit at release time.
	want := buildinfo.Get()
	out := buf.String()
	for _, field := range []struct {
		name, value string
	}{
		{"name", want.Name},
		{"version", want.Version},
		{"commit", want.Commit},
	} {
		if !strings.Contains(out, field.value) {
			t.Errorf("--version output missing %s %q\noutput: %s", field.name, field.value, out)
		}
	}
}

// alwaysFailWriter drives a real write failure through run's stderr
// parameter, a genuine seam: it is just an io.Writer a caller supplies.
// internal/ops's handleMetrics and handleStatus buffer internally, so
// nothing there can drive one without a production seam.
type alwaysFailWriter struct{}

func (alwaysFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("simulated write failure")
}

func TestRunVersionReportsAWriteFailure(t *testing.T) {
	// The --version branch must not discard this error: a write failure
	// would otherwise print nothing and still report success.
	err := run(context.Background(), []string{"--version"}, alwaysFailWriter{})
	if err == nil {
		t.Fatal("run(--version) with a failing writer = nil, want an error")
	}
	if !strings.Contains(err.Error(), "writing version information") {
		t.Errorf("run(--version) error = %v, want it to name what failed", err)
	}
}

func TestRunServesAndShutsDownCleanly(t *testing.T) {
	// A full startup on an ephemeral ops port, with the API listener bound
	// to loopback. The test asserts that the agent comes up, answers on the
	// ops listener, and exits when its context is cancelled.
	dir := t.TempDir()
	certFile, keyFile := writeTestKeyPair(t, dir)
	cfg := `
api:
  bind: { addresses: ["127.0.0.1"], port: 18443 }
  tls: { cert_file: ` + certFile + `, key_file: ` + keyFile + ` }
  auth:
    token: { enabled: true, hashes: ["sha256:` + strings.Repeat("a", 64) + `"] }
  access: { allow: ["127.0.0.1/32"] }
ops:
  bind: { addresses: ["127.0.0.1"], port: 19100 }
log: { level: error }
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	errCh, cancel := startRun(t, []string{"--config", path}, io.Discard)

	waitForStatus(t, "http://127.0.0.1:19100/status")

	resp, err := http.Get("http://127.0.0.1:19100/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("status body: %v", err)
	}
	if status["name"] != "hwi-api" {
		t.Errorf("status = %v", status)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

// TestRunServesPlainHTTPWithTLSAndAuthBothDisabled exercises the case this
// change adds: an operator who deliberately sets api.tls.enabled: false and
// api.auth.enabled: false (say, because a trusted local proxy already
// terminates TLS and authenticates callers) still gets a working agent
// rather than a refused start. Before this change, config.Validate rejected
// the missing api.tls.cert_file/key_file and the absence of any enabled
// auth mechanism as hard errors, so this configuration could never reach
// run's listener-serving code at all.
//
// It uses ports distinct from TestRunServesAndShutsDownCleanly so the two
// tests cannot collide when run in parallel or in the same package process.
func TestRunServesPlainHTTPWithTLSAndAuthBothDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := `
api:
  bind: { addresses: ["127.0.0.1"], port: 18444 }
  tls: { enabled: false }
  auth: { enabled: false }
  access: { allow: ["127.0.0.1/32"] }
ops:
  bind: { addresses: ["127.0.0.1"], port: 19101 }
log: { level: error }
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	errCh, cancel := startRun(t, []string{"--config", path}, io.Discard)

	// The ops listener opens before the initial collection and the API
	// listener only after it, so wait for the API itself.
	waitForStatus(t, "http://127.0.0.1:18444/api/v1/inventory")

	// The point of this test: a plain (non-TLS) GET against the API
	// listener succeeds, with no credentials presented at all, because both
	// TLS and every auth mechanism were deliberately disabled.
	resp, err := http.Get("http://127.0.0.1:18444/api/v1/inventory")
	if err != nil {
		t.Fatalf("plain HTTP request to the API listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/v1/inventory = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("response body: %v", err)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

func waitForStatus(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never became reachable", url)
}

// writeTestKeyPair generates a self-signed ECDSA certificate for 127.0.0.1
// and writes it, with its key, into dir. The agent's TLS configuration is
// loaded with tls.LoadX509KeyPair, which only needs a well-formed pair; the
// address is filled in so the same pair also works for a handshake.
func writeTestKeyPair(t *testing.T, dir string) (certPath, keyPath string) {
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
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

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

// The helper subcommand is dispatched before the daemon's flags, and run
// outside socket activation it refuses.
func TestRunDispatchesTheHelperSubcommand(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	err := run(context.Background(), []string{"helper"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "socket activation") {
		t.Errorf("run helper = %v, want a socket activation error", err)
	}
}

// serveHelper answers helper requests from root on a Unix socket and
// returns the socket's path.
func serveHelper(t *testing.T, root fs.FS) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hwi") // short: macOS caps socket paths at 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = helper.Handler{Root: root, ReadTimeout: time.Second}.Serve(c)
			}()
		}
	}()
	return sock
}

// Every subsystem has exactly one prober, and no prober is registered for a
// name that is not a subsystem.
func TestBuildProbersCoversEverySubsystem(t *testing.T) {
	t.Setenv("HWI_API_HELPER_SOCKET", "")
	probers := buildProbers(inventory.NewStore(map[apiv1.Subsystem]bool{}), fstest.MapFS{}, slog.New(slog.DiscardHandler))

	for _, s := range apiv1.Subsystems() {
		if probers[s] == nil {
			t.Errorf("no prober registered for subsystem %q", s)
		}
	}
	for s := range probers {
		if _, ok := apiv1.ParseSubsystem(string(s)); !ok {
			t.Errorf("prober registered for unknown subsystem %q", s)
		}
	}
	if got, want := len(probers), len(apiv1.Subsystems()); got != want {
		t.Errorf("%d probers registered, want %d, one per subsystem", got, want)
	}
}

func TestBuildProbersReadsTheSMBIOSTableThroughTheHelper(t *testing.T) {
	sock := serveHelper(t, fstest.MapFS{"sys/firmware/dmi/tables/DMI": {Data: []byte{127, 4, 0, 0, 0, 0}}})
	root := fstest.MapFS{"proc/meminfo": {Data: []byte("MemTotal: 1024 kB\n")}}
	probeModulesError := func() string {
		probers := buildProbers(inventory.NewStore(map[apiv1.Subsystem]bool{}), root, slog.New(slog.DiscardHandler))
		v, err := probers[apiv1.SubsystemMemory].Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return v.(*inventory.Memory).ModulesError
	}

	t.Setenv("HWI_API_HELPER_SOCKET", sock)
	if e := probeModulesError(); e != "" {
		t.Errorf("with the helper, modules_error = %q, want none", e)
	}
	t.Setenv("HWI_API_HELPER_SOCKET", "")
	if e := probeModulesError(); !strings.Contains(e, "the privileged helper is not configured") {
		t.Errorf("without the helper, modules_error = %q", e)
	}
}

// The daemon cannot read product_serial itself, so with the helper the
// serial number is the helper's; without it the daemon reads the host root
// only, which here has no product_serial, so the field is omitted.
func TestBuildProbersReadsTheRootOnlyDMIFieldsThroughTheHelper(t *testing.T) {
	sock := serveHelper(t, fstest.MapFS{"sys/class/dmi/id/product_serial": {Data: []byte("SVC123\n")}})
	root := fstest.MapFS{"sys/class/dmi/id/product_name": {Data: []byte("PowerEdge R760\n")}}
	probeProduct := func() inventory.Product {
		probers := buildProbers(inventory.NewStore(map[apiv1.Subsystem]bool{}), root, slog.New(slog.DiscardHandler))
		v, err := probers[apiv1.SubsystemProduct].Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return v.(inventory.Product)
	}

	t.Setenv("HWI_API_HELPER_SOCKET", sock)
	if p := probeProduct(); p.SerialNumber != "SVC123" || p.Name != "PowerEdge R760" || !p.Identifiers["serial_number"].Usable {
		t.Errorf("with the helper, product = %+v, want the helper's serial number", p)
	}
	t.Setenv("HWI_API_HELPER_SOCKET", "")
	if p := probeProduct(); p.SerialNumber != "" || p.Name != "PowerEdge R760" {
		t.Errorf("without the helper, product = %+v, want serial_number omitted", p)
	}
}
