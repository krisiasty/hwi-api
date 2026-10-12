package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

func TestResolvePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if err := os.WriteFile(filepath.Join(dir, "tls_key.pem"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}

	got, err := ResolvePath("credential:tls_key.pem")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if got != filepath.Join(dir, "tls_key.pem") {
		t.Errorf("ResolvePath = %q", got)
	}

	if _, err := ResolvePath("credential:absent"); err == nil {
		t.Error("a credential that is not present must be an error")
	}
	if _, err := ResolvePath("credential:../escape"); err == nil {
		t.Error("a credential name containing a path separator must be rejected")
	}

	if got, err := ResolvePath("/etc/hwi-api/ca.pem"); err != nil || got != "/etc/hwi-api/ca.pem" {
		t.Errorf("an absolute path must pass through unchanged: %q, %v", got, err)
	}
}

// TestPackagingAgreesOnTheTLSDirectory keeps the three places that name the
// TLS credentials in step: the unit loads /etc/hwi-api/tls as a directory
// (a missing directory would stop systemd starting the unit at all), the
// postinstall script creates it, and the shipped configuration names the
// credentials systemd derives from its files.
func TestPackagingAgreesOnTheTLSDirectory(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", "packaging", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const dir = "/etc/hwi-api/tls"

	loads := regexp.MustCompile(`(?m)^LoadCredential=(.*)$`).FindAllStringSubmatch(read("hwi-api.service"), -1)
	if len(loads) != 1 || strings.TrimSuffix(loads[0][1], "/") != "tls:"+dir {
		t.Errorf("unit LoadCredential= lines = %v, want exactly one loading the directory, tls:%s", loads, dir)
	}
	postinstall := read("postinstall.sh")
	if !strings.Contains(postinstall, "tls_dir="+dir+"\n") ||
		!regexp.MustCompile(`(?m)^install -d .*"\$tls_dir"$`).MatchString(postinstall) {
		t.Errorf("postinstall.sh does not create %s with install -d", dir)
	}
	config := read("config.yaml")
	for _, want := range []string{"credential:tls_cert.pem", "credential:tls_key.pem"} {
		if !strings.Contains(config, want) {
			t.Errorf("config.yaml does not mention %s", want)
		}
	}
}

func TestResolvePathWithoutCredentialsDirectory(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := ResolvePath("credential:tls_key.pem"); err == nil {
		t.Fatal("a credential reference without CREDENTIALS_DIRECTORY must be an error")
	}
}

// A credential loaded from a directory is named after its file, dot and
// all; "." and ".." on their own are still rejected.
func TestResolvePathDotsInNames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if err := os.WriteFile(filepath.Join(dir, "tls_key.pem"), []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}

	if got, err := ResolvePath("credential:tls_key.pem"); err != nil || got != filepath.Join(dir, "tls_key.pem") {
		t.Errorf("ResolvePath(tls_key.pem) = %q, %v", got, err)
	}
	for _, name := range []string{".", ".."} {
		if _, err := ResolvePath("credential:" + name); err == nil {
			t.Errorf("credential name %q must be rejected", name)
		}
	}
}

func TestResolvePathRejectsAnOverlongName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	name := strings.Repeat("a", 65)
	if _, err := ResolvePath("credential:" + name); err == nil {
		t.Fatal("a 65-character credential name must be rejected")
	}
}

func TestResolvePathNeverEchoesARejectedName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	// A JWT-shaped value is exactly what an operator might paste into a
	// credential: field by mistake; the error must not repeat any of it.
	secret := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.live-token-value"
	_, err := ResolvePath("credential:" + secret)
	if err == nil {
		t.Fatal("a malformed credential name must be rejected")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		t.Errorf("error echoes the rejected name in full: %v", err)
	}
	for i := 0; i+8 <= len(secret); i++ {
		if strings.Contains(msg, secret[i:i+8]) {
			t.Errorf("error echoes a substring of the rejected name: %v", err)
			break
		}
	}
}

// A token pasted where a credential name belongs can pass the name rule:
// the 64-character hex token the README generates does. So no error may
// repeat the name, valid or not; the caller's field prefix identifies it.
func TestResolvePathNeverEchoesAValidName(t *testing.T) {
	token := strings.Repeat("3f9a", 16) // 64 hex characters, a valid credential name
	for _, tc := range []struct {
		name, dir, want string
	}{
		{"not present", t.TempDir(), "not present"},
		{"no credentials directory", "", "CREDENTIALS_DIRECTORY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CREDENTIALS_DIRECTORY", tc.dir)
			_, err := ResolvePath("credential:" + token)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
			if strings.Contains(err.Error(), token[:8]) {
				t.Errorf("error echoes the credential name: %v", err)
			}
		})
	}
}

// Validation names the field instead of the credential.
func TestValidateNamesTheFieldOfAMissingCredential(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	cfg := validConfig(t)
	cfg.API.TLS.CertFile = "credential:tls_cert.pem"
	_, err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "api.tls.cert_file") || strings.Contains(err.Error(), "tls_cert.pem") {
		t.Errorf("error = %v, want one naming api.tls.cert_file but not the credential", err)
	}
}

func validConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	for _, f := range []string{cert, key} {
		if err := os.WriteFile(f, []byte("x"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Defaults()
	cfg.API.TLS.CertFile = cert
	cfg.API.TLS.KeyFile = key
	cfg.API.Auth.Token.Enabled = true
	cfg.API.Auth.Token.Hashes = []string{"sha256:" + strings.Repeat("a", 64)}
	cfg.API.Access.Allow = []string{"10.0.0.0/8"}
	return cfg
}

func TestValidateRejectsBrokenConfigurations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"token enabled with no digests", func(c *Config) { c.API.Auth.Token.Hashes = nil }, "digest"},
		{"malformed digest", func(c *Config) { c.API.Auth.Token.Hashes = []string{"plaintext-token"} }, "sha256:"},
		{"digest of the wrong length", func(c *Config) { c.API.Auth.Token.Hashes = []string{"sha256:abcd"} }, "64"},
		{"missing certificate", func(c *Config) { c.API.TLS.CertFile = "/nonexistent/cert.pem" }, "cert_file"},
		{"unsupported tls version", func(c *Config) { c.API.TLS.MinVersion = "1.1" }, "min_version"},
		{"zero port", func(c *Config) { c.API.Bind.Port = 0 }, "port"},
		{"port out of range", func(c *Config) { c.API.Bind.Port = 70000 }, "port"},
		{"negative refresh interval", func(c *Config) { c.Inventory.Refresh.Interval = Duration(-time.Second) }, "interval"},
		// http.Server reads a negative timeout, and a zero read or write
		// timeout, as no deadline at all; a zero read_header or idle falls
		// back to read. None is a deadline the operator wrote down.
		{"negative read_header timeout", func(c *Config) { c.API.Timeouts.ReadHeader = Duration(-time.Second) }, "api.timeouts.read_header must"},
		{"negative read timeout", func(c *Config) { c.API.Timeouts.Read = Duration(-time.Second) }, "api.timeouts.read must"},
		{"negative write timeout", func(c *Config) { c.API.Timeouts.Write = Duration(-time.Second) }, "api.timeouts.write must"},
		{"negative idle timeout", func(c *Config) { c.API.Timeouts.Idle = Duration(-time.Second) }, "api.timeouts.idle must"},
		{"zero read_header timeout", func(c *Config) { c.API.Timeouts.ReadHeader = 0 }, "api.timeouts.read_header must"},
		{"zero read timeout", func(c *Config) { c.API.Timeouts.Read = 0 }, "api.timeouts.read must"},
		{"zero write timeout", func(c *Config) { c.API.Timeouts.Write = 0 }, "api.timeouts.write must"},
		{"zero idle timeout", func(c *Config) { c.API.Timeouts.Idle = 0 }, "api.timeouts.idle must"},
		{"malformed allow entry", func(c *Config) { c.API.Access.Allow = []string{"10.0.0.0/64"} }, "allow"},
		{"mtls without a ca", func(c *Config) { c.API.Auth.MTLS.Enabled = true }, "client_ca_file"},
		{"mtls ca not a valid credential", func(c *Config) {
			c.API.Auth.MTLS.Enabled = true
			c.API.Auth.MTLS.ClientCAFile = "credential:not/valid"
		}, "api.auth.mtls.client_ca_file: credential name is not a valid identifier"},
		{"mtls ca missing", func(c *Config) {
			c.API.Auth.MTLS.Enabled = true
			c.API.Auth.MTLS.ClientCAFile = "/nonexistent/client-ca.pem"
		}, "api.auth.mtls.client_ca_file: stat /nonexistent/client-ca.pem"},
		{"auth enabled with no mechanism selected", func(c *Config) { c.API.Auth.Token.Enabled = false }, "authentication"},
		{"both listeners on one address and port", func(c *Config) {
			c.API.Bind.Addresses = []string{"127.0.0.1"}
			c.API.Bind.Port = 9100
			c.Ops.Bind.Port = 9100
		}, "same address and port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.mutate(&cfg)
			if _, err := cfg.Validate(); err == nil {
				t.Fatal("expected an error")
			} else if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q should mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestValidCredentialName(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"letters, digits, underscore, hyphen and dot", "TLS_cert-2.pem", true},
		{"a dot alone among others", ".hidden", true},
		{"the longest allowed", strings.Repeat("a", 64), true},
		{"empty", "", false},
		{"dot", ".", false},
		{"dot dot", "..", false},
		{"one too long", strings.Repeat("a", 65), false},
		{"a slash", "tls/cert.pem", false},
		{"a backslash", `tls\cert.pem`, false},
		{"a space", "tls cert.pem", false},
		{"base64 padding", "eyJhbGciOi==", false},
		{"a non-ASCII letter", "certé", false},
		{"invalid UTF-8", "cert\xff", false},
		{"a trailing newline", "cert.pem\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validCredentialName(tc.in); got != tc.want {
				t.Errorf("validCredentialName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidDigestHex(t *testing.T) {
	good := strings.Repeat("0123456789abcdef", 4)
	for _, tc := range []struct {
		name string
		hex  string
		want bool
	}{
		{"64 lowercase hex characters", good, true},
		{"empty", "", false},
		{"one short", good[:63], false},
		{"one long", good + "0", false},
		{"a non-hex letter", good[:63] + "g", false},
		{"uppercase hex", strings.ToUpper(good), false},
		{"a space", " " + good[1:], false},
		{"a multi-byte character", good[:62] + "é", false},
		{"a trailing newline", good[:63] + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validDigestHex(tc.hex); got != tc.want {
				t.Errorf("validDigestHex(%q) = %v, want %v", tc.hex, got, tc.want)
			}
		})
	}
}

// A malformed digest may be a live token pasted into the wrong field, and
// the error ends up on stderr and in the journal, so it names the entry by
// position and must not contain any of its characters.
func TestValidateDoesNotEchoAMalformedDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, entry, wantSub string }{
		{"no sha256 prefix", "s3cr3tLiveToken0123456789", "never a plaintext token"},
		{"not hex after the prefix", "sha256:s3cr3tLiveToken0123456789", "64 lowercase hex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.API.Auth.Token.Hashes = []string{good, tc.entry}
			_, err := cfg.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "api.auth.token.hashes[1]") || !strings.Contains(msg, tc.wantSub) {
				t.Errorf("error %q should name hashes[1] and say %q", msg, tc.wantSub)
			}
			if strings.Contains(msg, "s3c") { // not even a prefix of the entry
				t.Errorf("error %q echoes part of the entry", msg)
			}
		})
	}
}

func TestValidateAcceptsAGoodConfiguration(t *testing.T) {
	cfg := validConfig(t)
	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

func TestValidateWarnsAboutAnEmptyAllowList(t *testing.T) {
	cfg := validConfig(t)
	cfg.API.Access.Allow = nil

	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("an empty allow list is valid configuration: %v", err)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, " "), "reject every") {
		t.Errorf("warnings = %v, want one saying the API will reject every client", warnings)
	}
}

func TestValidateTLSDisabledSkipsCertKeyValidation(t *testing.T) {
	cfg := validConfig(t)
	cfg.API.TLS.Enabled = false
	// Point at files that do not exist at all: if Validate still checked
	// them, this would fail with a "cert_file"/"key_file" error rather than
	// the plaintext warning this test expects.
	cfg.API.TLS.CertFile = "/nonexistent/cert.pem"
	cfg.API.TLS.KeyFile = "/nonexistent/key.pem"

	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v, want TLS disabled to skip cert/key validation entirely", err)
	}
	if !strings.Contains(strings.Join(warnings, " "), "plaintext") {
		t.Errorf("warnings = %v, want one saying the listener serves plaintext", warnings)
	}
}

func TestValidateAuthDisabledWarnsNotErrors(t *testing.T) {
	cfg := validConfig(t)
	cfg.API.Auth.Enabled = false

	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v, want api.auth.enabled: false to warn, not error", err)
	}
	if !strings.Contains(strings.Join(warnings, " "), "api.auth.enabled is false") {
		t.Errorf("warnings = %v, want one naming api.auth.enabled as false", warnings)
	}
}

func TestValidateRejectsAuthEnabledWithNoAuthBlockAtAll(t *testing.T) {
	// The regression this whole feature exists to prevent: an operator who
	// forgets, mistypes or deletes the api.auth section entirely must not
	// end up with a silently unauthenticated API. api.auth.enabled defaults
	// to true even when the whole block is absent, so this must still be a
	// hard error, not a warning.
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	for _, f := range []string{cert, key} {
		if err := os.WriteFile(f, []byte("x"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	src := fmt.Sprintf(`api:
  tls:
    cert_file: %s
    key_file: %s
  access:
    allow: ["10.0.0.0/8"]
`, cert, key)

	cfg, err := Decode(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !cfg.API.Auth.Enabled {
		t.Fatal("api.auth.enabled should default to true when the whole auth block is absent")
	}

	if _, err := cfg.Validate(); err == nil {
		t.Fatal("a config with no auth block at all - and so no auth mechanism enabled - must be rejected " +
			"while api.auth.enabled defaults to true, not accepted with a warning")
	} else if !strings.Contains(err.Error(), "authentication") {
		t.Errorf("error %q should mention authentication", err)
	}
}

func TestValidateWarnsMoreStronglyWhenTLSAndAuthAreBothDisabled(t *testing.T) {
	cfg := validConfig(t)
	cfg.API.TLS.Enabled = false
	cfg.API.Auth.Enabled = false

	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	joined := strings.Join(warnings, " ")
	if !strings.Contains(joined, "cleartext") {
		t.Errorf("warnings = %v, want a distinct warning about serving inventory in cleartext with no auth", warnings)
	}
	if !strings.Contains(joined, "serial") || !strings.Contains(joined, "UUID") {
		t.Errorf("warnings = %v, want the combined warning to name what is exposed (serials and UUIDs)", warnings)
	}
}

// TestValidateRejectsBrokenConfigurations already covers both mechanism-
// specific requirements that remain hard errors regardless of
// api.auth.enabled: "mtls without a ca" (api.auth.mtls.client_ca_file
// required once mtls.enabled is true) and "token enabled with no digests"
// (a digest required once token.enabled is true). No standalone tests
// duplicate those cases here.

func TestValidateWarnsWhenTopologyLagsItsInputs(t *testing.T) {
	cfg := validConfig(t)
	cfg.Inventory.Refresh.Subsystems = map[apiv1.Subsystem]SubsystemConfig{
		apiv1.SubsystemTopology: {Interval: Duration(time.Hour)},
		apiv1.SubsystemCPU:      {Interval: Duration(time.Minute)},
	}

	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !strings.Contains(strings.Join(warnings, " "), "topology") {
		t.Errorf("warnings = %v, want one about topology lagging cpu", warnings)
	}
}

// TestValidateNegativeSubsystemIntervalNamesCanonicalFirst guards against
// the map-range nondeterminism the ruling on this file's cert/key and
// access-list checks was written to eliminate: with two subsystems both
// broken, the reported one must not depend on map iteration order.
func TestValidateNegativeSubsystemIntervalNamesCanonicalFirst(t *testing.T) {
	cfg := validConfig(t)
	cfg.Inventory.Refresh.Subsystems = map[apiv1.Subsystem]SubsystemConfig{
		apiv1.SubsystemMemory: {Interval: Duration(-time.Second)},
		apiv1.SubsystemCPU:    {Interval: Duration(-time.Second)},
	}

	_, err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "subsystems.cpu.interval") {
		t.Errorf("error %q should name cpu, the canonically-first of the two broken subsystems", err)
	}
}

func TestLoadReadsAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log:\n  level: warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := Load(path)
	if err == nil && cfg.Log.Level != "warn" {
		t.Errorf("log level = %q, want warn", cfg.Log.Level)
	}
	// Validation will fail on the missing TLS material; that is expected
	// here. The point of this test is that the file was read at all.
	if err != nil && !strings.Contains(err.Error(), "cert_file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	if _, _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing configuration file must be an error")
	}
}

// ReadFile strips a credential's resolved path from read errors but keeps
// a plain path, which is the operator's own setting.
func TestReadFileRedactsOnlyCredentialPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	name := strings.Repeat("3f9a", 16)
	if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFile("credential:" + name)
	if err == nil || strings.Contains(err.Error(), name[:8]) || strings.Contains(err.Error(), dir) {
		t.Errorf("credential read error = %v, want one without the name or path", err)
	}

	plain := filepath.Join(t.TempDir(), "ca.pem")
	if _, err := ReadFile(plain); err == nil || !strings.Contains(err.Error(), plain) {
		t.Errorf("plain read error = %v, want one naming %s", err, plain)
	}
}
