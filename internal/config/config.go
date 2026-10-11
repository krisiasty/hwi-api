// Package config defines the agent's configuration and how it is loaded.
package config

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	yaml "go.yaml.in/yaml/v3"
)

// Duration is a time.Duration written as a Go duration string, such as
// "5m". YAML has no duration scalar, and an unadorned number would be
// ambiguous about its unit.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("must be a duration string such as \"30s\" or \"5m\"")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: it needs a unit, such as \"30s\" or \"5m\"", s)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the whole configuration file.
type Config struct {
	API       APIConfig       `yaml:"api"`
	Ops       OpsConfig       `yaml:"ops"`
	Inventory InventoryConfig `yaml:"inventory"`
	Log       LogConfig       `yaml:"log"`
}

// APIConfig configures the authenticated inventory listener.
type APIConfig struct {
	Bind                  BindConfig    `yaml:"bind"`
	TLS                   TLSConfig     `yaml:"tls"`
	Auth                  AuthConfig    `yaml:"auth"`
	Access                AccessConfig  `yaml:"access"`
	Timeouts              TimeoutConfig `yaml:"timeouts"`
	MaxConcurrentRequests int           `yaml:"max_concurrent_requests"`
}

// OpsConfig configures the unauthenticated operational listener.
type OpsConfig struct {
	Bind   BindConfig   `yaml:"bind"`
	Access AccessConfig `yaml:"access"`
}

// BindConfig selects what a listener binds to. Addresses and interfaces may
// be mixed; both empty means every address.
type BindConfig struct {
	Addresses  []string `yaml:"addresses"`
	Interfaces []string `yaml:"interfaces"`
	Port       int      `yaml:"port"`
}

// TLSConfig configures the API listener's TLS. Paths accept a
// credential: reference.
type TLSConfig struct {
	// Enabled defaults to true: TLS is on unless an operator deliberately
	// turns it off, the realistic case being a trusted local proxy that
	// already terminates TLS in front of this agent. CertFile and KeyFile
	// are validated only while this is true.
	Enabled    bool   `yaml:"enabled"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	MinVersion string `yaml:"min_version"`
}

// AuthConfig holds both mechanisms. Either one passing admits a request.
//
// Enabled defaults to true, mirroring TLSConfig.Enabled: authentication is
// enforced unless an operator deliberately turns it off. While true, at
// least one of MTLS.Enabled or Token.Enabled must also be true, or
// Validate rejects the contradiction of "authentication enforced, but no
// mechanism configured to enforce" as an error rather than a warning. This
// is what keeps an absent or mistyped auth block from silently meaning
// "no authentication": omission must not be the insecure path.
type AuthConfig struct {
	Enabled bool        `yaml:"enabled"`
	MTLS    MTLSConfig  `yaml:"mtls"`
	Token   TokenConfig `yaml:"token"`
}

// MTLSConfig configures client certificate verification.
type MTLSConfig struct {
	Enabled         bool     `yaml:"enabled"`
	ClientCAFile    string   `yaml:"client_ca_file"`
	AllowedSubjects []string `yaml:"allowed_subjects"`
}

// TokenConfig configures bearer token verification. Only digests are ever
// configured; the tokens themselves live with whoever issues them.
type TokenConfig struct {
	Enabled    bool     `yaml:"enabled"`
	Hashes     []string `yaml:"hashes"`
	HashesFile string   `yaml:"hashes_file"`
}

// AccessConfig is a listener's address lists. Deny is evaluated first.
type AccessConfig struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// TimeoutConfig bounds request handling.
type TimeoutConfig struct {
	ReadHeader Duration `yaml:"read_header"`
	Read       Duration `yaml:"read"`
	Write      Duration `yaml:"write"`
	Idle       Duration `yaml:"idle"`
}

// InventoryConfig configures collection.
type InventoryConfig struct {
	Refresh RefreshConfig `yaml:"refresh"`
}

// RefreshConfig configures the background refresh loop. Interval is the
// floor for every subsystem's refresh interval; see EffectiveInterval.
type RefreshConfig struct {
	Interval   Duration                            `yaml:"interval"`
	Subsystems map[apiv1.Subsystem]SubsystemConfig `yaml:"subsystems"`
}

// SubsystemConfig overrides one subsystem's schedule. A nil Enabled means
// the subsystem keeps its default, which is on; a zero Interval means it
// keeps its default interval.
type SubsystemConfig struct {
	Enabled  *bool    `yaml:"enabled"`
	Interval Duration `yaml:"interval"`
}

// IsEnabled reports whether the subsystem should be collected.
func (s SubsystemConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// LogConfig configures logging.
type LogConfig struct {
	Level string `yaml:"level"`
}

// Defaults returns the configuration used when the file says nothing.
func Defaults() Config {
	return Config{
		API: APIConfig{
			// 8096 pairs with the ops port, 9096, and is free in the
			// Prometheus exporter port list and at IANA, unlike 8443, the
			// most common alternative HTTPS port.
			Bind: BindConfig{Port: 8096},
			TLS:  TLSConfig{Enabled: true, MinVersion: "1.3"},
			Auth: AuthConfig{Enabled: true},
			Timeouts: TimeoutConfig{
				ReadHeader: Duration(5 * time.Second),
				Read:       Duration(10 * time.Second),
				Write:      Duration(30 * time.Second),
				Idle:       Duration(60 * time.Second),
			},
			MaxConcurrentRequests: 32,
		},
		Ops: OpsConfig{
			Bind: BindConfig{
				Addresses: []string{"127.0.0.1", "::1"},
				// 9096 is free in the Prometheus exporter port list and at
				// IANA, unlike 9100, node_exporter's port, which is taken
				// on most monitored hosts.
				Port: 9096,
			},
			Access: AccessConfig{Allow: []string{"127.0.0.1/32", "::1/128"}},
		},
		Inventory: InventoryConfig{
			Refresh: RefreshConfig{Interval: Duration(time.Minute)},
		},
		Log: LogConfig{Level: "info"},
	}
}

// Decode reads YAML over the defaults. Decoding is strict: an unrecognised
// key is an error, and so is a second YAML document with content, because
// a silently ignored setting is worse than a refused start.
func Decode(r io.Reader) (Config, error) {
	cfg := Defaults()

	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty file is legitimate: everything defaults.
			return cfg, nil
		}
		return Config{}, fmt.Errorf("parsing configuration: %w", err)
	}
	if err := rejectFurtherDocuments(dec); err != nil {
		return Config{}, err
	}

	names := make([]string, 0, len(cfg.Inventory.Refresh.Subsystems))
	for name := range cfg.Inventory.Refresh.Subsystems {
		names = append(names, string(name))
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := apiv1.ParseSubsystem(name); !ok {
			return Config{}, fmt.Errorf("inventory.refresh.subsystems: %q is not a known subsystem", name)
		}
	}

	return cfg, nil
}

// rejectFurtherDocuments reads whatever follows the configuration document
// and fails if any further document has content. Decode reads only the
// first document, so without this every setting after a "---" line would
// be silently ignored, unknown keys included. A document with no content,
// such as a trailing "---" or one holding only comments, cannot carry a
// setting, so it is accepted.
func rejectFurtherDocuments(dec *yaml.Decoder) error {
	for n := 2; ; n++ {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parsing configuration: YAML document %d: %w", n, err)
		}
		if !emptyDocument(&doc) {
			return fmt.Errorf("parsing configuration: the file contains more than one YAML document (document %d, "+
				"line %d); put every setting in a single document, since only the first would be read", n, doc.Line)
		}
	}
}

// emptyDocument reports whether doc holds nothing but a null value, which
// is how the decoder represents a document with no content.
func emptyDocument(doc *yaml.Node) bool {
	if doc.Kind != yaml.DocumentNode {
		return false
	}
	for _, c := range doc.Content {
		if c.Kind != yaml.ScalarNode || c.Tag != "!!null" {
			return false
		}
	}
	return true
}
