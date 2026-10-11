package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

// cancellingAPI serves a store with every subsystem enabled and collected,
// through a refresh callback that cancels the request on its first call,
// as a client disconnecting during the first probe would, and records
// every subsystem it was called for.
func cancellingAPI(t *testing.T) (http.Handler, *[]apiv1.Subsystem, *http.Request) {
	t.Helper()
	enabled := map[apiv1.Subsystem]bool{}
	for _, s := range apiv1.Subsystems() {
		enabled[s] = true
	}
	store := inventory.NewStore(enabled)
	for _, s := range apiv1.Subsystems() {
		_ = store.Put(s, map[string]any{"ok": true}, collected)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var called []apiv1.Subsystem
	a := New(store, "", func(_ context.Context, s apiv1.Subsystem) {
		called = append(called, s)
		cancel()
	})
	mux := http.NewServeMux()
	a.Routes(mux)
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	return mux, &called, r
}

// TestCanceledSnapshotRefreshStopsAndRendersNothing: once the
// request is canceled during the first refresh, the snapshot handler must
// not start refreshes for the remaining subsystems, and must not build and
// encode a response for a client that has gone.
func TestCanceledSnapshotRefreshStopsAndRendersNothing(t *testing.T) {
	h, called, r := cancellingAPI(t)
	r.URL.Path, r.URL.RawQuery = "/api/v1/inventory", "refresh=true"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if len(*called) != 1 {
		t.Errorf("refresh callback ran for %v, want only the first subsystem", *called)
	}
	if w.Body.Len() != 0 || w.Header().Get("ETag") != "" {
		t.Errorf("a canceled request rendered a response: ETag %q, body %s", w.Header().Get("ETag"), w.Body)
	}
}

// TestCanceledSubsystemRefreshRendersNothing: the single-subsystem handler
// likewise builds no response once its forced refresh returns canceled.
func TestCanceledSubsystemRefreshRendersNothing(t *testing.T) {
	h, called, r := cancellingAPI(t)
	r.URL.Path, r.URL.RawQuery = "/api/v1/inventory/cpu", "refresh=true"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if len(*called) != 1 {
		t.Errorf("refresh callback ran for %v, want cpu once", *called)
	}
	if w.Body.Len() != 0 || w.Header().Get("ETag") != "" {
		t.Errorf("a canceled request rendered a response: ETag %q, body %s", w.Header().Get("ETag"), w.Body)
	}
}

// A request that is not canceled still gets its full response: the checks
// must not fire on the normal path.
func TestUncanceledRefreshStillRenders(t *testing.T) {
	h, called, r := cancellingAPI(t)
	r = r.WithContext(context.Background())
	r.URL.Path, r.URL.RawQuery = "/api/v1/inventory", "refresh=true"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if len(*called) != len(apiv1.Subsystems()) {
		t.Errorf("refresh callback ran for %d subsystems, want all %d", len(*called), len(apiv1.Subsystems()))
	}
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Errorf("status = %d, body length %d, want a rendered 200", w.Code, w.Body.Len())
	}
}
