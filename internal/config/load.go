package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/access"
)

// credentialPrefix marks a path that systemd supplies at run time.
const credentialPrefix = "credential:"

// digestHexLen is the length of the hex part of the only accepted token
// digest form, sha256:<64 lowercase hex characters>.
const digestHexLen = 64

// credentialNameMaxLen is systemd's own limit on a credential identifier.
const credentialNameMaxLen = 64

// credentialNamePattern and digestHexPattern are the characters a
// credential name and a token digest's hex part may hold. Their lengths are
// checked against the constants above, before the pattern scans anything.
var (
	credentialNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	digestHexPattern      = regexp.MustCompile(`^[0-9a-f]+$`)
)

// validCredentialName reports whether name is a systemd credential
// identifier: letters, digits, underscore, hyphen and dot, capped at
// credentialNameMaxLen, and neither "." nor "..". The dot is there for
// credentials loaded from a directory, which systemd names after the file,
// such as tls_cert.pem.
// A name that fails this is rejected without ever being echoed. This is
// what keeps a live secret out of the log if an operator pastes one where a
// credential name belongs, such as cert_file: credential:eyJhbGciOi...: the
// charset and length rules reject it on shape alone, before it is ever
// printed, and with "." and ".." they also cover the path-traversal
// rejections ("", ".", "..", "/", "\").
func validCredentialName(name string) bool {
	return len(name) <= credentialNameMaxLen && name != "." && name != ".." && credentialNamePattern.MatchString(name)
}

// validDigestHex reports whether s is exactly digestHexLen lowercase hex
// characters.
func validDigestHex(s string) bool {
	return len(s) == digestHexLen && digestHexPattern.MatchString(s)
}

// ResolvePath turns a configured path into a filesystem path. A
// credential:<name> reference resolves under $CREDENTIALS_DIRECTORY, which
// keeps secret material out of the configuration file entirely.
//
// Its errors never repeat the name, valid or not: a token pasted where a
// credential name belongs can pass the name rule (a 64-character hex token
// does), and the error goes to stderr and the journal. Callers prefix the
// configuration field instead, which identifies the entry just as well.
func ResolvePath(s string) (string, error) {
	if !strings.HasPrefix(s, credentialPrefix) {
		return s, nil
	}
	name := strings.TrimPrefix(s, credentialPrefix)
	if !validCredentialName(name) {
		// Report only the length: name may be a live secret an operator
		// pasted into the wrong field, and none of it is safe to echo.
		return "", fmt.Errorf("credential name is not a valid identifier "+
			"(%d characters; expected letters, digits, \"_\", \"-\" or \".\", up to 64)", len(name))
	}
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return "", errors.New("a credential: reference needs $CREDENTIALS_DIRECTORY, which is unset; " +
			"add a LoadCredential= entry to the unit or use an absolute path")
	}
	full := filepath.Join(dir, name)
	if _, err := os.Stat(full); err != nil {
		// Unwrap the *fs.PathError: its path ends in the name.
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
		return "", fmt.Errorf("the credential it names is not present in $CREDENTIALS_DIRECTORY (%w); "+
			"the packaged unit loads each file in /etc/hwi-api/tls as a credential named tls_<file name>", err)
	}
	return full, nil
}

// ReadFile reads the file a configured path or credential: reference names.
// For a credential its errors, like ResolvePath's, never contain the
// resolved path, which ends in the credential's name; a plain path is the
// operator's own setting and stays in them.
func ReadFile(ref string) ([]byte, error) {
	path, err := ResolvePath(ref)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, redactCredentialPath(ref, err)
	}
	return b, nil
}

// redactCredentialPath removes the path from an *fs.PathError when ref is
// a credential: reference, keeping the operation and its cause.
func redactCredentialPath(ref string, err error) error {
	if !strings.HasPrefix(ref, credentialPrefix) {
		return err
	}
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return fmt.Errorf("%s of the credential: %w", pe.Op, pe.Err)
	}
	return err
}

// Load reads, decodes and validates a configuration file. It returns any
// warnings alongside the configuration; warnings do not prevent startup.
func Load(path string) (Config, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, nil, fmt.Errorf("opening configuration: %w", err)
	}
	defer f.Close()

	cfg, err := Decode(f)
	if err != nil {
		return Config{}, nil, err
	}
	warnings, err := cfg.Validate()
	if err != nil {
		return Config{}, nil, err
	}
	return cfg, warnings, nil
}

// Validate rejects configurations that cannot produce a working agent, and
// returns warnings for those that are valid but likely to surprise.
func (c Config) Validate() ([]string, error) {
	var warnings []string

	if err := validatePort("api.bind.port", c.API.Bind.Port); err != nil {
		return nil, err
	}
	if err := validatePort("ops.bind.port", c.Ops.Bind.Port); err != nil {
		return nil, err
	}
	if err := c.validateListenerOverlap(); err != nil {
		return nil, err
	}

	switch c.API.TLS.MinVersion {
	case "1.2", "1.3":
	default:
		return nil, fmt.Errorf("api.tls.min_version %q: want \"1.2\" or \"1.3\"", c.API.TLS.MinVersion)
	}
	if c.API.TLS.Enabled {
		if err := validateFilePath("api.tls.cert_file", c.API.TLS.CertFile); err != nil {
			return nil, err
		}
		if err := validateFilePath("api.tls.key_file", c.API.TLS.KeyFile); err != nil {
			return nil, err
		}
	} else {
		warnings = append(warnings, "api.tls.enabled is false: the API listener will serve plaintext HTTP; "+
			"only disable TLS when a trusted local proxy terminates it in front of this agent")
	}

	if c.API.Auth.Enabled {
		// api.auth.enabled defaults to true, including when the whole auth
		// block is absent, so an operator who forgets, mistypes or deletes
		// this section gets a hard error here rather than a silently
		// unauthenticated listener: omission must not be the insecure path.
		//
		// mTLS only works when this listener terminates TLS itself: without
		// TLS no request carries a client certificate, so an enabled mTLS
		// block is inert. It then counts as no mechanism, and, as for any
		// inactive mechanism, its CA file is not required.
		mtlsActive := c.API.Auth.MTLS.Enabled && c.API.TLS.Enabled
		if !mtlsActive && !c.API.Auth.Token.Enabled {
			if c.API.Auth.MTLS.Enabled {
				return nil, fmt.Errorf("api.auth.mtls is the only enabled authentication mechanism, but " +
					"api.tls.enabled is false: client certificates can only be presented over TLS terminated " +
					"by this listener, so no client could ever authenticate; enable api.tls, enable " +
					"api.auth.token, or set api.auth.enabled: false")
			}
			return nil, fmt.Errorf("api.auth.enabled is true but no authentication mechanism is enabled: " +
				"set api.auth.mtls.enabled or api.auth.token.enabled, or set api.auth.enabled: false to " +
				"disable authentication deliberately")
		}
		if c.API.Auth.MTLS.Enabled && !c.API.TLS.Enabled {
			warnings = append(warnings, "api.auth.mtls.enabled is true but has no effect because api.tls.enabled "+
				"is false: no request can carry a client certificate, so only api.auth.token authenticates")
		}
		if mtlsActive {
			if c.API.Auth.MTLS.ClientCAFile == "" {
				return nil, fmt.Errorf("api.auth.mtls.client_ca_file is required when mTLS is enabled")
			}
			resolved, err := ResolvePath(c.API.Auth.MTLS.ClientCAFile)
			if err != nil {
				return nil, fmt.Errorf("api.auth.mtls.client_ca_file: %w", err)
			}
			if _, err := os.Stat(resolved); err != nil {
				return nil, fmt.Errorf("api.auth.mtls.client_ca_file: %w", redactCredentialPath(c.API.Auth.MTLS.ClientCAFile, err))
			}
		}
		if c.API.Auth.Token.Enabled {
			if len(c.API.Auth.Token.Hashes) == 0 && c.API.Auth.Token.HashesFile == "" {
				return nil, fmt.Errorf("api.auth.token is enabled but no digest is configured: " +
					"set hashes or hashes_file")
			}
			for i, h := range c.API.Auth.Token.Hashes {
				if err := validateDigest(h); err != nil {
					return nil, fmt.Errorf("api.auth.token.hashes[%d]: %w", i, err)
				}
			}
		}
	} else {
		warnings = append(warnings, "api.auth.enabled is false: the API listener will admit any client "+
			"api.access.allow lets through, with no credential check at all")
		// No authenticator is built, so a mechanism left enabled here is
		// inert: its credentials are not required or checked. Say so, in
		// case an operator believes it is still protecting the listener.
		for _, m := range []struct {
			name    string
			enabled bool
		}{
			{"api.auth.mtls", c.API.Auth.MTLS.Enabled},
			{"api.auth.token", c.API.Auth.Token.Enabled},
		} {
			if m.enabled {
				warnings = append(warnings, m.name+".enabled is true but has no effect because api.auth.enabled is false")
			}
		}
	}

	if !c.API.TLS.Enabled && !c.API.Auth.Enabled {
		warnings = append(warnings, "api.tls.enabled and api.auth.enabled are both false: the API listener will "+
			"serve hardware inventory - including DMI serial numbers and hardware UUIDs - in cleartext, with no "+
			"authentication, to anything api.access.allow admits; this combination is appropriate only behind a "+
			"trusted proxy that both terminates TLS and authenticates callers, never on a routable interface")
	}

	if _, err := access.New(c.API.Access.Allow, c.API.Access.Deny); err != nil {
		return nil, fmt.Errorf("api.access: %w", err)
	}
	if _, err := access.New(c.Ops.Access.Allow, c.Ops.Access.Deny); err != nil {
		return nil, fmt.Errorf("ops.access: %w", err)
	}
	if len(c.API.Access.Allow) == 0 {
		warnings = append(warnings, "api.access.allow is empty, so the API listener will "+
			"reject every client; populate it to grant access")
	}

	if c.Inventory.Refresh.Interval <= 0 {
		return nil, fmt.Errorf("inventory.refresh.interval must be a positive duration")
	}
	for _, name := range apiv1.Subsystems() {
		sub, ok := c.Inventory.Refresh.Subsystems[name]
		if !ok {
			continue
		}
		if sub.Interval < 0 {
			return nil, fmt.Errorf("inventory.refresh.subsystems.%s.interval must not be negative", name)
		}
	}
	warnings = append(warnings, c.topologyWarnings()...)

	// http.Server reads a negative timeout, and a zero read or write
	// timeout, as no deadline at all, and a zero read_header or idle as
	// "use read". Header reads come before authentication and the
	// concurrency limit, so a missing deadline lets a slow or idle peer hold
	// a connection indefinitely. Only a positive duration is a deadline.
	for _, t := range []struct {
		field string
		value Duration
	}{
		{"api.timeouts.read_header", c.API.Timeouts.ReadHeader},
		{"api.timeouts.read", c.API.Timeouts.Read},
		{"api.timeouts.write", c.API.Timeouts.Write},
		{"api.timeouts.idle", c.API.Timeouts.Idle},
	} {
		if t.value <= 0 {
			return nil, fmt.Errorf("%s must be a positive duration", t.field)
		}
	}

	if c.API.MaxConcurrentRequests <= 0 {
		return nil, fmt.Errorf("api.max_concurrent_requests must be positive")
	}

	return warnings, nil
}

func validatePort(field string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s %d is not a valid port", field, port)
	}
	return nil
}

// validateFilePath checks that a configured path is present, resolves any
// credential: reference, and confirms the target exists. Callers check
// fields in a fixed order so a configuration with more than one broken
// path always names the same one on every run.
func validateFilePath(field, path string) error {
	if path == "" {
		return fmt.Errorf("%s is required", field)
	}
	resolved, err := ResolvePath(path)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if _, err := os.Stat(resolved); err != nil {
		return fmt.Errorf("%s: %w", field, redactCredentialPath(path, err))
	}
	return nil
}

// validateDigest checks h without echoing any of it: an operator may have
// pasted a live token where a digest belongs, and the error goes to stderr
// and the journal. The caller names the entry by its position instead.
func validateDigest(h string) error {
	if !strings.HasPrefix(h, "sha256:") {
		return errors.New("must be a digest of the form sha256:<64 hex characters>, never a plaintext token")
	}
	if !validDigestHex(strings.TrimPrefix(h, "sha256:")) {
		return errors.New("is not a sha256 digest: expected sha256: followed by 64 lowercase hex characters")
	}
	return nil
}

// validateListenerOverlap catches the two listeners colliding, which would
// otherwise surface as an opaque bind error from whichever started second.
func (c Config) validateListenerOverlap() error {
	if c.API.Bind.Port != c.Ops.Bind.Port {
		return nil
	}
	apiAddrs := c.API.Bind.Addresses
	opsAddrs := c.Ops.Bind.Addresses
	if len(apiAddrs) == 0 || len(opsAddrs) == 0 {
		return fmt.Errorf("api and ops listeners would use the same address and port (%d); "+
			"one of them binds every address", c.API.Bind.Port)
	}
	for _, a := range apiAddrs {
		if slices.Contains(opsAddrs, a) {
			return fmt.Errorf("api and ops listeners would use the same address and port (%s:%d)",
				a, c.API.Bind.Port)
		}
	}
	return nil
}

// topologyWarnings flags a topology subsystem refreshed less often than
// cpu or memory: topology is composed from the same per-node CPU lists and
// memory areas, so a change would show in cpu or memory first and look
// inconsistent with topology until it caught up. It compares effective
// intervals - defaults, overrides and the floor, exactly as scheduled -
// not only what the file sets, and only for subsystems that are enabled.
func (c Config) topologyWarnings() []string {
	r := c.Inventory.Refresh
	if !r.IsEnabled(apiv1.SubsystemTopology) {
		return nil
	}
	topo := r.EffectiveInterval(apiv1.SubsystemTopology)
	var out []string
	for _, input := range []apiv1.Subsystem{apiv1.SubsystemCPU, apiv1.SubsystemMemory} {
		if !r.IsEnabled(input) {
			continue
		}
		if in := r.EffectiveInterval(input); topo > in {
			out = append(out, fmt.Sprintf(
				"topology refreshes every %s but %s every %s (effective intervals, after defaults and the "+
					"inventory.refresh.interval floor); topology is composed from %s, so a change will appear there first",
				topo, input, in, input))
		}
	}
	return out
}
