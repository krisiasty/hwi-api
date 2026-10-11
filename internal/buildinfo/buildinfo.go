// Package buildinfo reports how this binary was built.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// Set by the linker via -ldflags at release time. An unstamped build (go
// build, go test) leaves them empty and falls back to the module's own
// recorded build information.
var (
	version string
	commit  string
	date    string
)

// Info describes the running binary.
type Info struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
}

// Get returns the build information for this binary.
func Get() Info { return For("hwi-api") }

// For returns the build information for the binary called name. Every
// binary built from this module is stamped with the same values.
func For(name string) Info {
	i := Info{
		Name:      name,
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: goVersion(),
	}
	if i.Version == "" || i.Commit == "" {
		fillFromDebug(&i)
	}
	if i.Version == "" {
		i.Version = "devel"
	}
	return i
}

// Short describes the build on one line: its version, commit and date.
func (i Info) Short() string {
	commit := i.Commit
	if commit == "" {
		commit = "unknown"
	} else if len(commit) > 7 {
		commit = commit[:7]
	}
	date := i.Date
	if date == "" {
		date = "unknown"
	}
	return fmt.Sprintf("%s (commit %s, built %s)", i.Version, commit, date)
}

func goVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.GoVersion != "" {
		return bi.GoVersion
	}
	return "unknown"
}

// fillFromDebug recovers what it can from the VCS stamps the toolchain
// embeds, so a `go build` still reports a usable commit.
func fillFromDebug(i *Info) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if i.Commit == "" {
				i.Commit = s.Value
			}
		case "vcs.time":
			if i.Date == "" {
				i.Date = s.Value
			}
		}
	}
	if i.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		i.Version = bi.Main.Version
	}
}
