package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

func topologyWarningsFor(t *testing.T, base time.Duration, subs map[apiv1.Subsystem]SubsystemConfig) []string {
	t.Helper()
	cfg := validConfig(t)
	cfg.Inventory.Refresh.Interval = Duration(base)
	cfg.Inventory.Refresh.Subsystems = subs
	warnings, err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var out []string
	for _, w := range warnings {
		if strings.Contains(w, "topology") {
			out = append(out, w)
		}
	}
	return out
}

// TestTopologyOnlyOverrideWarns: overriding only topology leaves cpu and
// memory at their defaults, which is exactly the drift the warning exists
// for, so the check compares effective intervals, not explicit settings.
func TestTopologyOnlyOverrideWarns(t *testing.T) {
	got := topologyWarningsFor(t, time.Minute, map[apiv1.Subsystem]SubsystemConfig{
		apiv1.SubsystemTopology: {Interval: Duration(30 * time.Minute)},
	})
	joined := strings.Join(got, " ")
	if len(got) != 2 || !strings.Contains(joined, "cpu") || !strings.Contains(joined, "memory") {
		t.Errorf("warnings = %v, want one each for cpu and memory at their 5m defaults", got)
	}
}

// With the floor applied, intervals configured differently can end up
// equal, and then there is no drift to warn about.
func TestFloorRaisedIntervalsDoNotWarn(t *testing.T) {
	got := topologyWarningsFor(t, 10*time.Minute, map[apiv1.Subsystem]SubsystemConfig{
		apiv1.SubsystemTopology: {Interval: Duration(6 * time.Minute)},
		apiv1.SubsystemCPU:      {Interval: Duration(2 * time.Minute)},
		apiv1.SubsystemMemory:   {Interval: Duration(2 * time.Minute)},
	})
	if len(got) != 0 {
		t.Errorf("warnings = %v, want none: every interval is raised to the 10m floor", got)
	}
}

// The floor can also create drift that the configured values alone hide.
func TestFloorRaisedTopologyWarns(t *testing.T) {
	got := topologyWarningsFor(t, 10*time.Minute, map[apiv1.Subsystem]SubsystemConfig{
		apiv1.SubsystemTopology: {Interval: Duration(20 * time.Minute)},
		apiv1.SubsystemCPU:      {Interval: Duration(time.Minute)},
		apiv1.SubsystemMemory:   {Interval: Duration(30 * time.Minute)},
	})
	if len(got) != 1 || !strings.Contains(got[0], "cpu") || !strings.Contains(got[0], "10m") {
		t.Errorf("warnings = %v, want one for cpu at its effective 10m", got)
	}
}

// A disabled input is never collected, so it cannot drift from topology.
func TestDisabledInputDoesNotWarn(t *testing.T) {
	off := false
	got := topologyWarningsFor(t, time.Minute, map[apiv1.Subsystem]SubsystemConfig{
		apiv1.SubsystemTopology: {Interval: Duration(30 * time.Minute)},
		apiv1.SubsystemCPU:      {Enabled: &off},
		apiv1.SubsystemMemory:   {Enabled: &off},
	})
	if len(got) != 0 {
		t.Errorf("warnings = %v, want none with cpu and memory disabled", got)
	}
}

func TestEffectiveInterval(t *testing.T) {
	r := RefreshConfig{
		Interval: Duration(2 * time.Minute),
		Subsystems: map[apiv1.Subsystem]SubsystemConfig{
			apiv1.SubsystemPCI:     {Interval: Duration(30 * time.Minute)},
			apiv1.SubsystemUdevDRM: {Interval: Duration(time.Second)},
			apiv1.SubsystemBIOS:    {},
		},
	}
	for sub, want := range map[apiv1.Subsystem]time.Duration{
		apiv1.SubsystemPCI:     30 * time.Minute, // override
		apiv1.SubsystemUdevDRM: 2 * time.Minute,  // override raised to the floor
		apiv1.SubsystemBIOS:    time.Hour,        // entry without an interval keeps the default
		apiv1.SubsystemLinks:   2 * time.Minute,  // 1m default raised to the floor
		apiv1.SubsystemCPU:     5 * time.Minute,  // no entry: default
	} {
		if got := r.EffectiveInterval(sub); got != want {
			t.Errorf("EffectiveInterval(%s) = %s, want %s", sub, got, want)
		}
	}
}

// Every subsystem has a default, so none is scheduled at the bare floor
// by accident.
func TestEverySubsystemHasADefaultInterval(t *testing.T) {
	for _, s := range apiv1.Subsystems() {
		if DefaultIntervals[s] <= 0 {
			t.Errorf("no default interval for %s", s)
		}
	}
}

// TestREADMEListsTheDefaultIntervals keeps the README's table of default
// intervals in step with DefaultIntervals: every subsystem appears exactly
// once, with its actual default.
func TestREADMEListsTheDefaultIntervals(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(b), "| Subsystems")
	if start < 0 {
		t.Fatal(`README has no default-interval table starting "| Subsystems"`)
	}
	table := string(b)[start:]
	table = table[:strings.Index(table, "\n\n")]

	row := regexp.MustCompile("^\\|(.*)\\|\\s*`([0-9a-z]+)`\\s*\\|$")
	name := regexp.MustCompile("`([a-z][a-z-]*)`")
	listed := map[apiv1.Subsystem]time.Duration{}
	for _, line := range strings.Split(table, "\n")[2:] {
		m := row.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			t.Fatalf("unrecognised row in the README default-interval table: %q", line)
		}
		d, err := time.ParseDuration(m[2])
		if err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		for _, n := range name.FindAllStringSubmatch(m[1], -1) {
			sub := apiv1.Subsystem(n[1])
			if _, dup := listed[sub]; dup {
				t.Errorf("%s is listed twice", sub)
			}
			listed[sub] = d
		}
	}
	for _, s := range apiv1.Subsystems() {
		if got, ok := listed[s]; !ok {
			t.Errorf("README does not list %s's default interval", s)
		} else if got != DefaultIntervals[s] {
			t.Errorf("README lists %s at %s, but its default is %s", s, got, DefaultIntervals[s])
		}
	}
	if len(listed) != len(apiv1.Subsystems()) {
		t.Errorf("README lists %d subsystems, want %d", len(listed), len(apiv1.Subsystems()))
	}
}
