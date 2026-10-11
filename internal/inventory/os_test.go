package inventory

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"
	"time"
)

const osRelease = `# A comment
NAME="Ubuntu"
VERSION="22.04.3 LTS (Jammy Jellyfish)"
ID=ubuntu
ID_LIKE=debian
PRETTY_NAME="Ubuntu 22.04.3 LTS"
VERSION_ID="22.04"
VERSION_CODENAME=jammy
CPE_NAME="cpe:/o:canonical:ubuntu_linux:22.04"

BUILD_ID=
QUOTED_ESCAPE="a \"quoted\" value"
SINGLE='single quoted'
`

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"etc/os-release":            {Data: []byte(osRelease)},
		"proc/sys/kernel/osrelease": {Data: []byte("5.15.0-101-generic\n")},
		"proc/sys/kernel/version":   {Data: []byte("#111-Ubuntu SMP Tue Mar 5 20:16:58 UTC 2026\n")},
		"proc/stat":                 {Data: []byte("cpu  1 2 3\nintr 9\nbtime 1758441600\nprocesses 42\n")},
	}
}

func probeOS(t *testing.T, fsys fstest.MapFS) OSInfo {
	t.Helper()
	p := NewOSProber(fsys,
		func() (string, error) { return "x86_64", nil },
		func() (string, error) { return "node1", nil })
	got, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	info, ok := got.(OSInfo)
	if !ok {
		t.Fatalf("Probe returned %T, want OSInfo", got)
	}
	return info
}

func TestOSProberReadsDistribution(t *testing.T) {
	info := probeOS(t, testFS())

	if info.Distribution.ID != "ubuntu" {
		t.Errorf("ID = %q", info.Distribution.ID)
	}
	if info.Distribution.PrettyName != "Ubuntu 22.04.3 LTS" {
		t.Errorf("PrettyName = %q; quotes must be stripped", info.Distribution.PrettyName)
	}
	if info.Distribution.VersionID != "22.04" {
		t.Errorf("VersionID = %q", info.Distribution.VersionID)
	}
	if info.Distribution.VersionCodename != "jammy" {
		t.Errorf("VersionCodename = %q", info.Distribution.VersionCodename)
	}
	if info.Distribution.CPEName != "cpe:/o:canonical:ubuntu_linux:22.04" {
		t.Errorf("CPEName = %q", info.Distribution.CPEName)
	}
}

func TestOSProberReadsKernelAndHost(t *testing.T) {
	info := probeOS(t, testFS())

	if info.Kernel.Release != "5.15.0-101-generic" {
		t.Errorf("Kernel.Release = %q", info.Kernel.Release)
	}
	if info.Kernel.Version != "#111-Ubuntu SMP Tue Mar 5 20:16:58 UTC 2026" {
		t.Errorf("Kernel.Version = %q", info.Kernel.Version)
	}
	if info.Kernel.Architecture != "x86_64" {
		t.Errorf("Kernel.Architecture = %q", info.Kernel.Architecture)
	}
	if info.Hostname != "node1" {
		t.Errorf("Hostname = %q", info.Hostname)
	}
}

func TestOSProberReadsBootTimeAsAnAbsoluteTimestamp(t *testing.T) {
	info := probeOS(t, testFS())

	want := time.Unix(1758441600, 0).UTC()
	if !info.BootTime.Equal(want) {
		t.Errorf("BootTime = %s, want %s", info.BootTime, want)
	}
}

func TestOSProberFallsBackToUsrLib(t *testing.T) {
	fsys := testFS()
	delete(fsys, "etc/os-release")
	fsys["usr/lib/os-release"] = &fstest.MapFile{Data: []byte("ID=rhel\nVERSION_ID=\"9.4\"\n")}

	info := probeOS(t, fsys)
	if info.Distribution.ID != "rhel" {
		t.Errorf("ID = %q, want the /usr/lib fallback to be used", info.Distribution.ID)
	}
}

func TestOSProberDegradesRatherThanFailing(t *testing.T) {
	// A host with none of the sources present must still answer rather than
	// fail the whole subsystem. Following the agent's convention, a string whose
	// source could not be read is "unknown", and a non-string field keeps
	// its zero value.
	info := probeOS(t, fstest.MapFS{})

	d := info.Distribution
	for name, got := range map[string]string{
		"distribution.id": d.ID, "distribution.name": d.Name, "distribution.version": d.Version,
		"distribution.version_id": d.VersionID, "distribution.version_codename": d.VersionCodename,
		"distribution.pretty_name": d.PrettyName, "distribution.build_id": d.BuildID,
		"distribution.variant": d.Variant, "distribution.variant_id": d.VariantID, "distribution.cpe_name": d.CPEName,
		"kernel.release": info.Kernel.Release, "kernel.version": info.Kernel.Version,
	} {
		if got != "unknown" {
			t.Errorf("%s = %q with its source unreadable, want \"unknown\"", name, got)
		}
	}
	if !info.BootTime.IsZero() {
		t.Errorf("BootTime = %s, want the zero time", info.BootTime)
	}
}

// A failed uname or hostname call is an unreadable source too.
func TestOSProberReportsFailedCallsAsUnknown(t *testing.T) {
	p := NewOSProber(testFS(),
		func() (string, error) { return "", errors.New("uname failed") },
		func() (string, error) { return "", errors.New("hostname failed") })
	got, err := p.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info := got.(OSInfo)
	if info.Kernel.Architecture != "unknown" || info.Hostname != "unknown" {
		t.Errorf("architecture = %q, hostname = %q, want both \"unknown\"", info.Kernel.Architecture, info.Hostname)
	}
}

// A source that was read but leaves a value unset keeps it "": only an
// unreadable source means "unknown".
func TestOSProberKeepsUnsetValuesEmpty(t *testing.T) {
	info := probeOS(t, testFS())
	if info.Distribution.Variant != "" {
		t.Errorf("Variant = %q for a key os-release does not set, want \"\"", info.Distribution.Variant)
	}
	if info.Distribution.BuildID != "" {
		t.Errorf("BuildID = %q for a key os-release sets empty, want \"\"", info.Distribution.BuildID)
	}
}

func TestParseOSRelease(t *testing.T) {
	got := parseOSRelease([]byte(osRelease))

	tests := map[string]string{
		"NAME":          "Ubuntu",
		"VERSION":       "22.04.3 LTS (Jammy Jellyfish)",
		"ID":            "ubuntu",
		"BUILD_ID":      "",
		"QUOTED_ESCAPE": `a "quoted" value`,
		"SINGLE":        "single quoted",
	}
	for k, want := range tests {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["# A comment"]; ok {
		t.Error("comments must not become keys")
	}
}

func TestParseOSReleaseIgnoresMalformedLines(t *testing.T) {
	got := parseOSRelease([]byte("GOOD=1\nnonsense\n=novalue\n  \nALSOGOOD=2\n"))
	if got["GOOD"] != "1" || got["ALSOGOOD"] != "2" {
		t.Errorf("valid lines were lost: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("malformed lines produced entries: %v", got)
	}
}
