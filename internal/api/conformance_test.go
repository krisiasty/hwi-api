package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenEqual compares a response body against a recorded golden file.
// A diff here is not a test to re-record without thought: these files are
// the published payload shape, and a change to one is an API change.
func goldenEqual(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	pretty.WriteByte('\n')

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file %s; run: go test ./internal/api/ -update", path)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Errorf("response differs from %s.\n--- want ---\n%s\n--- got ---\n%s", path, want, pretty.Bytes())
	}
}

func TestGoldenSubsystemResponse(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{
			"total_cores": 8, "processors": []any{map[string]any{"id": 0, "vendor": "GenuineIntel"}},
		}, collected)
	})
	goldenEqual(t, "subsystem_cpu.golden", get(t, h, "/api/v1/inventory/cpu", nil).Body.Bytes())
}

func TestGoldenSnapshotResponse(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemOS: true, apiv1.SubsystemUdevDRM: false}
	h := newTestAPI(t, enabled, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"total_cores": 8}, collected)
		_ = s.Put(apiv1.SubsystemOS, map[string]any{"distribution": map[string]any{"id": "ubuntu"}}, collected)
	})
	goldenEqual(t, "snapshot.golden", get(t, h, "/api/v1/inventory", nil).Body.Bytes())
}

func TestGoldenErrorResponse(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemUdevDRM: false}, nil)
	goldenEqual(t, "error_disabled.golden", get(t, h, "/api/v1/inventory/udev-drm", nil).Body.Bytes())
}

// TestOpenAPIListsEveryEndpoint keeps the published document honest about
// which paths exist, which is the part most likely to drift.
func TestOpenAPIListsEveryEndpoint(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("reading the published contract: %v", err)
	}

	for _, want := range []string{
		"/api/v1/inventory:",
		"/api/v1/inventory/{subsystem}:",
	} {
		if !bytes.Contains(doc, []byte(want)) {
			t.Errorf("openapi.yaml does not document %s", want)
		}
	}
	for _, s := range apiv1.Subsystems() {
		if !bytes.Contains(doc, []byte(string(s))) {
			t.Errorf("openapi.yaml does not mention subsystem %q in its enum", s)
		}
	}
	for _, code := range []string{"200", "304", "401", "403", "404", "405", "503"} {
		if !bytes.Contains(doc, []byte(`"`+code+`"`)) {
			t.Errorf("openapi.yaml does not document status %s", code)
		}
	}
}
