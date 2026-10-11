package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

var collected = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

func newTestAPI(t *testing.T, enabled map[apiv1.Subsystem]bool, seed func(*inventory.Store)) http.Handler {
	t.Helper()
	store := inventory.NewStore(enabled)
	if seed != nil {
		seed(store)
	}
	a := New(store, "b41d2f0c", func(context.Context, apiv1.Subsystem) {})

	mux := http.NewServeMux()
	a.Routes(mux)
	return mux
}

func get(t *testing.T, h http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) apiv1.Envelope {
	t.Helper()
	var env apiv1.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not an envelope: %v (%s)", err, w.Body)
	}
	return env
}

func TestSubsystemEndpointReturnsAnEnvelope(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
	})

	w := get(t, h, "/api/v1/inventory/cpu", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}

	env := decode(t, w)
	if env.SchemaVersion != apiv1.SchemaVersion || env.Subsystem != apiv1.SubsystemCPU {
		t.Errorf("envelope = %+v", env)
	}
	if !env.Complete {
		t.Error("a healthy single subsystem must be complete")
	}
	if !env.CollectedAt.Equal(collected) {
		t.Errorf("CollectedAt = %s", env.CollectedAt)
	}
	if string(env.Data) != `{"cores":8}` {
		t.Errorf("Data = %s", env.Data)
	}
	if got := w.Header().Get("ETag"); got == "" {
		t.Error("ETag header is missing")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

func TestLastGoodDataIsServedWithAnError(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemPCI: true}, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemPCI, map[string]any{"devices": 3}, collected)
		s.Fail(apiv1.SubsystemPCI, errors.New("sysfs unreadable"), collected.Add(time.Hour))
	})

	w := get(t, h, "/api/v1/inventory/pci", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: last-good data is still data", w.Code)
	}
	env := decode(t, w)
	if _, ok := env.Errors[apiv1.SubsystemPCI]; !ok {
		t.Error("a failing refresh must be reported in errors")
	}
	if !env.CollectedAt.Equal(collected) {
		t.Errorf("CollectedAt = %s, want the time of the last success", env.CollectedAt)
	}
}

func TestNeverCollectedReturns503(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemPCI: true}, func(s *inventory.Store) {
		s.Fail(apiv1.SubsystemPCI, errors.New("sysfs unreadable"), collected)
	})

	w := get(t, h, "/api/v1/inventory/pci", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body apiv1.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if body.Error.Code != apiv1.CodeSubsystemUnavailable {
		t.Errorf("code = %q", body.Error.Code)
	}
}

func TestDisabledSubsystemReturns404(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemUdevDRM: false}, nil)

	w := get(t, h, "/api/v1/inventory/udev-drm", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var body apiv1.ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != apiv1.CodeSubsystemDisabled {
		t.Errorf("code = %q, want %q", body.Error.Code, apiv1.CodeSubsystemDisabled)
	}
}

func TestUnknownSubsystemReturns404(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{}, nil)
	if w := get(t, h, "/api/v1/inventory/firmware", nil); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestSnapshotComposesEntriesAndFlagsIncompleteness(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{
		apiv1.SubsystemCPU: true, apiv1.SubsystemPCI: true, apiv1.SubsystemUdevDRM: false,
	}
	h := newTestAPI(t, enabled, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
		s.Fail(apiv1.SubsystemPCI, errors.New("nope"), collected)
	})

	w := get(t, h, "/api/v1/inventory", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	env := decode(t, w)
	if env.Subsystem != apiv1.SubsystemAll {
		t.Errorf("Subsystem = %q, want all", env.Subsystem)
	}
	if env.Complete {
		t.Error("pci has never collected, so the snapshot is not complete")
	}
	if !slices.Contains(env.Disabled, apiv1.SubsystemUdevDRM) {
		t.Errorf("Disabled = %v, want udev-drm present", env.Disabled)
	}
	if slices.Contains(env.Disabled, apiv1.SubsystemCPU) {
		t.Errorf("Disabled = %v, cpu is enabled and must not be listed", env.Disabled)
	}
	if slices.Contains(env.Disabled, apiv1.SubsystemPCI) {
		t.Errorf("Disabled = %v, pci is enabled and must not be listed", env.Disabled)
	}

	var data map[apiv1.Subsystem]apiv1.SnapshotEntry
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("snapshot data: %v", err)
	}
	if _, ok := data[apiv1.SubsystemCPU]; !ok {
		t.Error("cpu should be present")
	}
	if _, ok := data[apiv1.SubsystemPCI]; ok {
		t.Error("a subsystem with no data must be omitted from data")
	}
}

func TestSnapshotIsCompleteWhenOnlyDisabledSubsystemsAreAbsent(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemUdevDRM: false}
	h := newTestAPI(t, enabled, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
	})

	env := decode(t, get(t, h, "/api/v1/inventory", nil))
	if !env.Complete {
		t.Error("a disabled subsystem is intentionally absent and must not make a snapshot incomplete")
	}
}

func TestSnapshotReportsTheOldestCollectionTime(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemOS: true}
	h := newTestAPI(t, enabled, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{}, collected.Add(time.Hour))
		_ = s.Put(apiv1.SubsystemOS, map[string]any{}, collected)
	})

	env := decode(t, get(t, h, "/api/v1/inventory", nil))
	if !env.CollectedAt.Equal(collected) {
		t.Errorf("CollectedAt = %s, want the oldest so staleness is worst-case", env.CollectedAt)
	}
}

func TestSnapshotWithEverySubsystemDisabledReturns200(t *testing.T) {
	enabled := make(map[apiv1.Subsystem]bool, len(apiv1.Subsystems()))
	for _, s := range apiv1.Subsystems() {
		enabled[s] = false
	}
	h := newTestAPI(t, enabled, nil)

	w := get(t, h, "/api/v1/inventory", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a fully disabled host is a legitimate steady state", w.Code)
	}

	env := decode(t, w)
	if !env.Complete {
		t.Error("complete must be vacuously true when no subsystem is enabled")
	}
	if len(env.Disabled) != len(apiv1.Subsystems()) {
		t.Errorf("Disabled = %v, want every subsystem listed", env.Disabled)
	}
	for _, s := range apiv1.Subsystems() {
		if !slices.Contains(env.Disabled, s) {
			t.Errorf("Disabled = %v, missing %q", env.Disabled, s)
		}
	}
	if string(env.Data) != "{}" {
		t.Errorf("Data = %s, want an empty object", env.Data)
	}
}

func TestSnapshotWithEnabledSubsystemsButNoDataReturns503(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemPCI: true}
	h := newTestAPI(t, enabled, nil)

	w := get(t, h, "/api/v1/inventory", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: subsystems are enabled but none has ever collected", w.Code)
	}
	var body apiv1.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body: %v", err)
	}
	if body.Error.Code != apiv1.CodeSubsystemUnavailable {
		t.Errorf("code = %q", body.Error.Code)
	}
}

func TestSnapshotRefreshSkipsDisabledSubsystems(t *testing.T) {
	store := inventory.NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemUdevDRM: false})
	_ = store.Put(apiv1.SubsystemCPU, map[string]any{"cores": 2}, collected)

	var called []apiv1.Subsystem
	a := New(store, "", func(_ context.Context, s apiv1.Subsystem) {
		called = append(called, s)
	})
	mux := http.NewServeMux()
	a.Routes(mux)

	get(t, mux, "/api/v1/inventory?refresh=true", nil)

	if slices.Contains(called, apiv1.SubsystemUdevDRM) {
		t.Errorf("refresh callback ran for a disabled subsystem: called = %v", called)
	}
	if !slices.Contains(called, apiv1.SubsystemCPU) {
		t.Errorf("refresh callback should still run for an enabled subsystem: called = %v", called)
	}
}

func TestConditionalRequest(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
	})

	first := get(t, h, "/api/v1/inventory/cpu", nil)
	etag := first.Header().Get("ETag")

	match := get(t, h, "/api/v1/inventory/cpu", map[string]string{"If-None-Match": etag})
	if match.Code != http.StatusNotModified {
		t.Errorf("matching If-None-Match: status = %d, want 304", match.Code)
	}
	if match.Body.Len() != 0 {
		t.Errorf("304 must have no body, got %s", match.Body)
	}

	stale := get(t, h, "/api/v1/inventory/cpu", map[string]string{"If-None-Match": `"stale"`})
	if stale.Code != http.StatusOK {
		t.Errorf("stale If-None-Match: status = %d, want 200", stale.Code)
	}
}

func TestConditionalRequestWildcard(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
	})

	w := get(t, h, "/api/v1/inventory/cpu", map[string]string{"If-None-Match": "*"})
	if w.Code != http.StatusNotModified {
		t.Errorf("wildcard If-None-Match: status = %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 must have no body, got %s", w.Body)
	}
}

func TestConditionalRequestList(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
	})

	first := get(t, h, "/api/v1/inventory/cpu", nil)
	etag := first.Header().Get("ETag")

	list := get(t, h, "/api/v1/inventory/cpu", map[string]string{
		"If-None-Match": `"aaa", ` + etag + `, "bbb"`,
	})
	if list.Code != http.StatusNotModified {
		t.Errorf("comma-separated If-None-Match: status = %d, want 304", list.Code)
	}
}

func TestMethodsOtherThanGetAndHeadAreRejected(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, nil)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/inventory/cpu", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
}

func TestRefreshQueryForcesACollection(t *testing.T) {
	store := inventory.NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	_ = store.Put(apiv1.SubsystemCPU, map[string]any{"cores": 2}, collected)

	called := 0
	a := New(store, "", func(_ context.Context, s apiv1.Subsystem) {
		called++
		_ = store.Put(s, map[string]any{"cores": 16}, collected.Add(time.Minute))
	})
	mux := http.NewServeMux()
	a.Routes(mux)

	env := decode(t, get(t, mux, "/api/v1/inventory/cpu?refresh=true", nil))
	if called != 1 {
		t.Errorf("refresh callback ran %d times, want 1", called)
	}
	if string(env.Data) != `{"cores":16}` {
		t.Errorf("Data = %s, want the refreshed payload", env.Data)
	}
}
