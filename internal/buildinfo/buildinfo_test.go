package buildinfo

import (
	"strings"
	"testing"
)

func TestGetUsesLdflagValues(t *testing.T) {
	version, commit, date = "1.2.3", "abc1234", "2026-09-21T10:00:00Z"
	t.Cleanup(func() { version, commit, date = "", "", "" })

	got := Get()
	if got.Name != "hwi-api" {
		t.Errorf("Name = %q, want %q", got.Name, "hwi-api")
	}
	if got.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", got.Version, "1.2.3")
	}
	if got.Commit != "abc1234" {
		t.Errorf("Commit = %q, want %q", got.Commit, "abc1234")
	}
	if got.GoVersion == "" {
		t.Error("GoVersion is empty; it must always be populated")
	}
}

func TestGetFallsBackWhenUnstamped(t *testing.T) {
	version, commit, date = "", "", ""

	got := Get()
	if got.Version == "" {
		t.Error("Version is empty; an unstamped build must still report something")
	}
}

func TestForNamesTheBinary(t *testing.T) {
	version, commit, date = "1.2.3", "abc1234", "2026-09-21T10:00:00Z"
	t.Cleanup(func() { version, commit, date = "", "", "" })

	got := For("example-tool")
	if got.Name != "example-tool" || got.Version != "1.2.3" || got.Commit != "abc1234" {
		t.Errorf("For = %+v", got)
	}
}

func TestShort(t *testing.T) {
	i := Info{Version: "1.2.3", Commit: "0123456789abcdef", Date: "2026-09-21T10:00:00Z"}
	if got, want := i.Short(), "1.2.3 (commit 0123456, built 2026-09-21T10:00:00Z)"; got != want {
		t.Errorf("Short = %q, want %q", got, want)
	}
	if got, want := (Info{Version: "devel"}).Short(), "devel (commit unknown, built unknown)"; got != want {
		t.Errorf("Short = %q, want %q", got, want)
	}
	if got := (Info{Version: "1", Commit: "abc"}).Short(); !strings.Contains(got, "commit abc,") {
		t.Errorf("Short = %q, want a short commit kept whole", got)
	}
}
