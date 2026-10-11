// Package auth admits or refuses API requests.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/krisiasty/hwi-api/internal/config"
)

// Method names the mechanism that admitted a request, for logging and
// metrics.
type Method string

// The supported mechanisms.
const (
	MethodMTLS  Method = "mtls"
	MethodToken Method = "token"
)

// Sentinel errors. They distinguish "nothing was presented", which is a
// 401, from "something was presented and refused", which is a 403.
var (
	ErrNoCredentials = errors.New("no credentials presented")
	ErrRejected      = errors.New("credentials rejected")
)

// Authenticator checks a request against both configured mechanisms.
type Authenticator struct {
	mtlsEnabled bool
	clientCAs   *x509.CertPool

	// allowedSubjects pins the certificates a verified chain may present,
	// beyond CA trust alone. Each entry has one of two shapes; see
	// subjectAllowed for what each one matches.
	allowedSubjects []string

	tokenEnabled bool
	digests      [][]byte
}

// New builds an authenticator from configuration.
func New(cfg config.AuthConfig) (*Authenticator, error) {
	a := &Authenticator{
		mtlsEnabled:     cfg.MTLS.Enabled,
		allowedSubjects: cfg.MTLS.AllowedSubjects,
		tokenEnabled:    cfg.Token.Enabled,
	}

	if a.mtlsEnabled {
		// config.ReadFile keeps a credential's resolved path, which ends in
		// its name, out of every error; the field prefix identifies it.
		pem, err := config.ReadFile(cfg.MTLS.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("mtls.client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("mtls.client_ca_file: the file contains no certificates")
		}
		a.clientCAs = pool
	}

	if a.tokenEnabled {
		digests, err := loadDigests(cfg.Token)
		if err != nil {
			return nil, err
		}
		a.digests = digests
	}
	return a, nil
}

// MTLSEnabled reports whether client certificates are checked, so the
// server knows which tls.ClientAuthType to use.
func (a *Authenticator) MTLSEnabled() bool { return a.mtlsEnabled }

// TokenEnabled reports whether bearer tokens are checked, mirroring
// MTLSEnabled for the other configured mechanism.
func (a *Authenticator) TokenEnabled() bool { return a.tokenEnabled }

// ClientCAs returns the pool used to verify client certificates.
func (a *Authenticator) ClientCAs() *x509.CertPool { return a.clientCAs }

// Authenticate admits the request if either mechanism accepts it.
//
// The error distinguishes absent credentials from refused ones, but the
// handler's response says no more than 401 or 403: which mechanism failed
// is not the caller's business.
func (a *Authenticator) Authenticate(r *http.Request) (Method, error) {
	presented := false

	if a.mtlsEnabled && r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		presented = true
		if a.subjectAllowed(r.TLS.VerifiedChains) {
			return MethodMTLS, nil
		}
	}

	if a.tokenEnabled {
		if token, ok := bearerToken(r); ok {
			presented = true
			if a.tokenAllowed(token) {
				return MethodToken, nil
			}
		}
	}

	if presented {
		return "", ErrRejected
	}
	return "", ErrNoCredentials
}

// subjectAllowed checks the pinned subject list.
//
// An empty list means any certificate the CA vouched for is acceptable. A
// non-empty entry has one of two shapes, so an operator can say precisely
// which they mean:
//
//   - A pin containing "=" is DN-shaped: it is matched against the whole
//     Subject distinguished name, and, when it names a single CN and
//     nothing else ("CN=value"), against the Subject common name. A pin
//     with further attributes ("CN=value,O=org") is a full DN and must
//     match the whole Subject DN: falling back to the CN would admit a
//     certificate whose CN merely spells out those attributes. A Subject
//     Alternative Name never satisfies a DN-shaped pin — narrowing to one
//     Subject identity is the point of writing a pin this way, and letting
//     any SAN the same CA vouched for also satisfy it would defeat that
//     narrowing.
//   - A pin with no "=" is a bare name: it is matched against the Subject
//     common name (exactly) or against any DNS Subject Alternative Name
//     (case-insensitively, since DNS names are).
func (a *Authenticator) subjectAllowed(chains [][]*x509.Certificate) bool {
	if len(a.allowedSubjects) == 0 {
		return true
	}
	for _, chain := range chains {
		if len(chain) == 0 {
			continue
		}
		leaf := chain[0]
		for _, want := range a.allowedSubjects {
			if strings.Contains(want, "=") {
				// DN-shaped: match the whole DN, or the CN when the pin
				// names a single CN. A SAN is deliberately never consulted
				// here.
				if leaf.Subject.String() == want {
					return true
				}
				if cn, ok := singleCNPin(want); ok && leaf.Subject.CommonName == cn {
					return true
				}
				continue
			}
			// Bare name: the Subject CN, or a DNS SAN compared
			// case-insensitively.
			if leaf.Subject.CommonName == want {
				return true
			}
			for _, dns := range leaf.DNSNames {
				if strings.EqualFold(dns, want) {
					return true
				}
			}
		}
	}
	return false
}

// singleCNPin reports whether a DN-shaped pin names a single CN and nothing
// else, and if so returns the CN value with its RFC 4514 escapes decoded.
// An unescaped "," or ";" separates RDNs and an unescaped "+" joins
// attributes within one, so either makes the pin a full DN rather than a
// single CN. A malformed escape is treated the same way: a pin that cannot
// be read unambiguously as a single CN gets no CN fallback.
func singleCNPin(pin string) (string, bool) {
	value, ok := strings.CutPrefix(pin, "CN=")
	if !ok {
		return "", false
	}
	var cn strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch c {
		case ',', ';', '+':
			return "", false
		case '\\':
			if i+2 < len(value) && isHex(value[i+1]) && isHex(value[i+2]) {
				b, _ := hex.DecodeString(value[i+1 : i+3])
				cn.WriteByte(b[0])
				i += 2
				continue
			}
			if i+1 >= len(value) {
				return "", false
			}
			cn.WriteByte(value[i+1])
			i++
		default:
			cn.WriteByte(c)
		}
	}
	return cn.String(), true
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// tokenAllowed compares digests in constant time. Tokens carry at least
// 256 bits of entropy, so a plain digest is appropriate: there is no
// low-entropy secret to protect against offline brute force.
func (a *Authenticator) tokenAllowed(token string) bool {
	sum := sha256.Sum256([]byte(token))
	match := 0
	for _, want := range a.digests {
		// Every candidate is compared, so timing does not reveal how far
		// down the list a match was found.
		match |= subtle.ConstantTimeCompare(sum[:], want)
	}
	return match == 1
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, value, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || value == "" {
		return "", false
	}
	return value, true
}

func loadDigests(cfg config.TokenConfig) ([][]byte, error) {
	raw := append([]string(nil), cfg.Hashes...)

	if cfg.HashesFile != "" {
		content, err := config.ReadFile(cfg.HashesFile)
		if err != nil {
			return nil, fmt.Errorf("token.hashes_file: %w", err)
		}
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			raw = append(raw, line)
		}
	}

	out := make([][]byte, 0, len(raw))
	for _, entry := range raw {
		hexPart := strings.TrimPrefix(entry, "sha256:")
		decoded, err := hex.DecodeString(hexPart)
		if err != nil || len(decoded) != sha256.Size {
			// The entry itself is never echoed: it may be a pasted token.
			return nil, errors.New("a configured token digest is not a valid sha256 digest")
		}
		out = append(out, decoded)
	}
	if len(out) == 0 {
		return nil, errors.New("token authentication is enabled but no digests were loaded")
	}
	return out, nil
}
