package inventory

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// OSInfo describes the operating system.
type OSInfo struct {
	Distribution Distribution `json:"distribution"`
	Kernel       Kernel       `json:"kernel"`
	Hostname     string       `json:"hostname"`
	// BootTime is absolute rather than an uptime duration, so that a value
	// sitting in the cache stays correct instead of ageing.
	BootTime time.Time `json:"boot_time"`
}

// Distribution is the os-release identity of the installation.
type Distribution struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	VersionID       string `json:"version_id"`
	VersionCodename string `json:"version_codename"`
	PrettyName      string `json:"pretty_name"`
	BuildID         string `json:"build_id"`
	Variant         string `json:"variant"`
	VariantID       string `json:"variant_id"`
	CPEName         string `json:"cpe_name"`
}

// Kernel describes the running kernel.
type Kernel struct {
	Release      string `json:"release"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
}

// NewOSProber collects OS facts from fsys. Sources are read relative to the
// filesystem root, so tests can substitute fstest.MapFS.
//
// A missing or unreadable source degrades its fields rather than failing
// the probe: an agent that refuses to report anything because
// /etc/os-release is absent is less useful than one reporting the kernel
// alone. Unreadable values follow one convention, so that every
// payload marks them the same way: a string whose source could not be read
// is unknown ("unknown"), a source that was read but leaves a value
// unset gives "", and a non-string field keeps its zero value, so BootTime
// is the zero time when /proc/stat is unreadable.
func NewOSProber(fsys fs.FS, uname, hostname func() (string, error)) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		info := OSInfo{}

		release := readFirst(fsys, "etc/os-release", "usr/lib/os-release")
		if release == nil {
			u := unknown
			info.Distribution = Distribution{
				ID: u, Name: u, Version: u, VersionID: u, VersionCodename: u,
				PrettyName: u, BuildID: u, Variant: u, VariantID: u, CPEName: u,
			}
		} else {
			kv := parseOSRelease(release)
			info.Distribution = Distribution{
				ID:              kv["ID"],
				Name:            kv["NAME"],
				Version:         kv["VERSION"],
				VersionID:       kv["VERSION_ID"],
				VersionCodename: kv["VERSION_CODENAME"],
				PrettyName:      kv["PRETTY_NAME"],
				BuildID:         kv["BUILD_ID"],
				Variant:         kv["VARIANT"],
				VariantID:       kv["VARIANT_ID"],
				CPEName:         kv["CPE_NAME"],
			}
		}

		info.Kernel.Release = readTrimmed(fsys, "proc/sys/kernel/osrelease")
		info.Kernel.Version = readTrimmed(fsys, "proc/sys/kernel/version")
		info.Kernel.Architecture = orUnknown(uname())
		info.Hostname = orUnknown(hostname())
		info.BootTime = bootTime(fsys)

		return info, nil
	})
}

// parseOSRelease decodes the shell-like key=value format. Values may be
// bare, single quoted, or double quoted with backslash escapes.
func parseOSRelease(b []byte) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = unquote(strings.TrimSpace(value))
	}
	return out
}

// unquote strips shell quoting. Inside double quotes a backslash escapes
// the next character; inside single quotes nothing is special.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	switch {
	case s[0] == '\'' && s[len(s)-1] == '\'':
		return s[1 : len(s)-1]
	case s[0] == '"' && s[len(s)-1] == '"':
		inner := s[1 : len(s)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				i++
				b.WriteByte(inner[i])
				continue
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	default:
		return s
	}
}

// bootTime reads the btime line of /proc/stat, which is the absolute time
// the kernel started.
func bootTime(fsys fs.FS) time.Time {
	b, err := fs.ReadFile(fsys, "proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.Unix(secs, 0).UTC()
	}
	return time.Time{}
}

func readFirst(fsys fs.FS, names ...string) []byte {
	for _, n := range names {
		if b, err := fs.ReadFile(fsys, n); err == nil {
			return b
		}
	}
	return nil
}

// readTrimmed returns name's whitespace-trimmed content, or unknown if
// it cannot be read.
func readTrimmed(fsys fs.FS, name string) string {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return unknown
	}
	return strings.TrimSpace(string(b))
}

// orUnknown returns s, or unknown if err is non-nil.
func orUnknown(s string, err error) string {
	if err != nil {
		return unknown
	}
	return s
}
