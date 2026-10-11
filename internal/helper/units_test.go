package helper

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// unitDirectives reads a packaged systemd unit into its directives, by
// key, in file order. Sections are not told apart; no key used here
// appears in two.
func unitDirectives(t *testing.T, name string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "packaging", name))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == '[' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = append(out[k], v)
		}
	}
	return out
}

func wantDirectives(t *testing.T, unit string, d map[string][]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if !slices.Equal(d[k], []string{v}) {
			t.Errorf("%s: %s = %q, want exactly %q", unit, k, d[k], v)
		}
	}
}

func TestHelperSocketUnit(t *testing.T) {
	d := unitDirectives(t, "hwi-api-helper.socket")
	wantDirectives(t, "hwi-api-helper.socket", d, map[string]string{
		"ListenStream":   "/run/hwi-api-helper.sock",
		"SocketUser":     "root",
		"SocketGroup":    "hwi-api",
		"SocketMode":     "0660",
		"Accept":         "yes",
		"MaxConnections": "4",
		"RemoveOnStop":   "yes",
	})
}

// The helper holds CAP_DAC_READ_SEARCH and nothing else, and sees almost
// no files to use it on.
func TestHelperServiceIsConfined(t *testing.T) {
	d := unitDirectives(t, "hwi-api-helper@.service")
	wantDirectives(t, "hwi-api-helper@.service", d, map[string]string{
		"ExecStart":               "/usr/bin/hwi-api helper",
		"User":                    "hwi-api-helper",
		"Group":                   "hwi-api-helper",
		"AmbientCapabilities":     "CAP_DAC_READ_SEARCH",
		"CapabilityBoundingSet":   "CAP_DAC_READ_SEARCH",
		"NoNewPrivileges":         "yes",
		"TemporaryFileSystem":     "/:ro",
		"BindReadOnlyPaths":       "/sys:/sys:norbind /sys/fs/cgroup /usr/bin/hwi-api /etc/passwd",
		"ProtectProc":             "invisible",
		"ProcSubset":              "pid",
		"PrivateDevices":          "yes",
		"PrivateNetwork":          "yes",
		"RestrictAddressFamilies": "AF_UNIX",
		"RuntimeMaxSec":           "60",
		"CollectMode":             "inactive-or-failed",
		"IPAddressDeny":           "any",
		"ProtectKernelTunables":   "yes",
		"ProtectKernelModules":    "yes",
		"ProtectKernelLogs":       "yes",
		"ProtectControlGroups":    "yes",
		"ProtectClock":            "yes",
		"ProtectHostname":         "yes",
		"RestrictSUIDSGID":        "yes",
		"RestrictRealtime":        "yes",
		"RestrictNamespaces":      "yes",
		"LockPersonality":         "yes",
		"MemoryDenyWriteExecute":  "yes",
		"SystemCallArchitectures": "native",
		"SystemCallFilter":        "@system-service",
	})
}

func TestAgentUnitUsesTheHelper(t *testing.T) {
	d := unitDirectives(t, "hwi-api.service")
	socket := unitDirectives(t, "hwi-api-helper.socket")["ListenStream"]
	if len(socket) != 1 || !slices.Contains(d["Environment"], "HWI_API_HELPER_SOCKET="+socket[0]) {
		t.Errorf("Environment = %q, want HWI_API_HELPER_SOCKET=%v", d["Environment"], socket)
	}
	for _, k := range []string{"Wants", "After"} {
		if !strings.Contains(strings.Join(d[k], " "), "hwi-api-helper.socket") {
			t.Errorf("%s = %q, want it to name hwi-api-helper.socket", k, d[k])
		}
	}
	if len(d["ExecStartPre"]) != 0 {
		t.Errorf("ExecStartPre = %q; the startup copies are gone", d["ExecStartPre"])
	}
	wantDirectives(t, "hwi-api.service", d, map[string]string{"AmbientCapabilities": "", "CapabilityBoundingSet": ""})
}
