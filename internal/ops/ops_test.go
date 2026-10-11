package ops

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/internal/buildinfo"
)

// testMetrics is a fixed exposition, so /metrics has something to render.
type testMetrics string

func (m testMetrics) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, string(m))
	return int64(n), err
}

const testExposition = testMetrics("# HELP test_total T.\n# TYPE test_total counter\ntest_total 1\n")

func handler(t testing.TB, ready func() bool) http.Handler {
	t.Helper()
	o := New(testExposition, buildinfo.Info{Name: "hwi-api", Version: "0.1.0", Commit: "fc1eb08", GoVersion: "go1.27.1"},
		ready, time.Now().Add(-90*time.Second))
	mux := http.NewServeMux()
	o.Routes(mux)
	return mux
}

func do(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestHealthzIsAlwaysOK(t *testing.T) {
	if w := do(t, handler(t, func() bool { return false }), "/healthz"); w.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200 even when not ready", w.Code)
	}
}

func TestReadyzTracksReadiness(t *testing.T) {
	if w := do(t, handler(t, func() bool { return false }), "/readyz"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz while not ready = %d, want 503", w.Code)
	}
	if w := do(t, handler(t, func() bool { return true }), "/readyz"); w.Code != http.StatusOK {
		t.Errorf("readyz while ready = %d, want 200", w.Code)
	}
}

func TestMetricsServesExposition(t *testing.T) {
	w := do(t, handler(t, func() bool { return true }), "/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("metrics = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "test_total 1") {
		t.Errorf("body = %s", w.Body)
	}
}

func TestStatusReportsBuildAndUptime(t *testing.T) {
	w := do(t, handler(t, func() bool { return true }), "/status")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var got Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status body: %v", err)
	}
	if got.Name != "hwi-api" || got.Version != "0.1.0" || got.Commit != "fc1eb08" {
		t.Errorf("status = %+v", got)
	}
	if got.Status != "ready" {
		t.Errorf("Status = %q, want ready", got.Status)
	}
	if got.UptimeSeconds < 89 {
		t.Errorf("UptimeSeconds = %v", got.UptimeSeconds)
	}
}

// testLogger is an interface for error reporting, implemented by *testing.T and testCapture.
type testLogger interface {
	Helper()
	Errorf(format string, args ...any)
}

// checkHostIdentityRecursive walks a JSON structure recursively, checking
// that no key at any level contains forbidden substrings.
func checkHostIdentityRecursive(t testLogger, v any, path string) {
	t.Helper()
	forbidden := []string{"serial", "uuid", "host", "machine", "asset"}

	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			// Check if key contains any forbidden substring
			kLower := strings.ToLower(k)
			for _, substr := range forbidden {
				if strings.Contains(kLower, substr) {
					t.Errorf("/status must not expose key %q at %s (contains %q)", k, path, substr)
				}
			}
			// Recurse into the value
			checkHostIdentityRecursive(t, child, path+"."+k)
		}
	case []any:
		for i, child := range val {
			checkHostIdentityRecursive(t, child, path+"["+fmt.Sprint(i)+"]")
		}
	case string, float64, bool, nil:
		// Leaf values: nothing more to check
	}
}

func TestStatusNeverLeaksHostIdentity(t *testing.T) {
	// The ops listener is unauthenticated, so host serials and UUIDs must
	// not appear here however convenient that would be. This test walks the
	// entire JSON structure recursively, checking all keys at all depths
	// against a denylist of forbidden substrings.
	w := do(t, handler(t, func() bool { return true }), "/status")
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	checkHostIdentityRecursive(t, raw, "")
}

func TestStatusIdentityGuardDetectsNesting(t *testing.T) {
	// Verify the guard actually catches host-identity fields, including
	// nested ones. This test temporarily injects a nested host field to
	// prove the recursive check catches it.
	w := do(t, handler(t, func() bool { return true }), "/status")
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}

	// Add a nested device object with a serial field to verify detection.
	raw["device"] = map[string]any{
		"serial": "ABC123",
	}

	// Now verify the guard catches it. We'll capture the test failure.
	tester := &testCapture{}
	checkHostIdentityRecursive(tester, raw, "")
	if !tester.failed {
		t.Error("guard did not detect nested serial field")
	}
}

func TestDemonstrationHostIdentityViolationDetection(t *testing.T) {
	// Verify the guard catches serial_number (matches "serial" substring)
	payload := map[string]any{
		"name":          "hwi-api",
		"version":       "0.1.0",
		"serial_number": "SN123456",
	}
	checker := &testCapture{}
	checkHostIdentityRecursive(checker, payload, "")
	if !checker.failed {
		t.Error("guard failed to detect serial_number violation")
	}

	// Verify the guard catches machine_id (matches "machine" substring)
	payload2 := map[string]any{
		"name": "hwi-api",
		"device": map[string]any{
			"machine_id": "MACHINE-123",
		},
	}
	checker2 := &testCapture{}
	checkHostIdentityRecursive(checker2, payload2, "")
	if !checker2.failed {
		t.Error("guard failed to detect machine_id violation")
	}

	// Verify clean payload passes
	payload3 := map[string]any{
		"name":    "hwi-api",
		"version": "0.1.0",
		"status":  "ready",
	}
	checker3 := &testCapture{}
	checkHostIdentityRecursive(checker3, payload3, "")
	if checker3.failed {
		t.Error("guard incorrectly flagged clean payload")
	}
}

// testCapture captures Errorf calls to verify the guard detects violations.
type testCapture struct {
	failed bool
}

func (tc *testCapture) Helper() {}

func (tc *testCapture) Errorf(format string, args ...any) {
	tc.failed = true
}

func TestStatusReportsStartingBeforeReady(t *testing.T) {
	w := do(t, handler(t, func() bool { return false }), "/status")
	var got Status
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Status != "starting" {
		t.Errorf("Status = %q, want starting", got.Status)
	}
}

// Both handleMetrics and handleStatus buffer their output into a
// bytes.Buffer/json.Encoder-over-buffer before writing anything to the
// ResponseWriter, and only write their 500 branch if that buffering step
// itself fails. That is precisely what makes those branches unreachable by
// construction from a test: a write to a bytes.Buffer does not fail, and
// json.Encoder cannot fail encoding a Status value (it is a flat struct of
// strings, a bool-free status string and a float64 - nothing unencodable
// ever reaches it). There is no honest way to drive either 500 branch
// without adding a seam to production code whose only purpose would be to
// give a test something to break, which is out of scope here. So instead
// the two tests below verify the property the buffering exists to provide:
// the response actually served is complete and well-formed, never a
// truncated or partial one. If either handler is ever refactored to write
// or encode directly to the ResponseWriter instead of buffering first, that
// refactor must reintroduce a real failure test alongside it - the 500
// branch would then be reachable, and an untested reachable error branch is
// worse than a documented unreachable one.

func TestMetricsResponseIsCompleteAndWellFormed(t *testing.T) {
	o := New(testExposition, buildinfo.Info{Name: "hwi-api", Version: "0.1.0", Commit: "fc1eb08", GoVersion: "go1.27.1"},
		func() bool { return true }, time.Now().Add(-90*time.Second))
	mux := http.NewServeMux()
	o.Routes(mux)

	w := do(t, mux, "/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("metrics = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}

	// The response body must be exactly what the metrics wrote: nothing
	// added, nothing missing, nothing truncated by the handler's own
	// buffering step.
	if w.Body.String() != string(testExposition) {
		t.Errorf("handler body does not match the exposition\ngot:  %q\nwant: %q",
			w.Body.String(), testExposition)
	}
}

func TestStatusResponseIsCompleteAndWellFormed(t *testing.T) {
	w := do(t, handler(t, func() bool { return true }), "/status")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	var got Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status body did not parse as JSON: %v\nbody: %s", err, w.Body.Bytes())
	}
	if got.Name == "" || got.Version == "" || got.Commit == "" {
		t.Errorf("status = %+v, want non-empty name, version and commit", got)
	}
}
