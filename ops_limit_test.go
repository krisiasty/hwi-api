package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/krisiasty/hwi-api/internal/access"
	"github.com/krisiasty/hwi-api/internal/buildinfo"
	"github.com/krisiasty/hwi-api/internal/inventory"
	"github.com/krisiasty/hwi-api/internal/metrics"
	"github.com/krisiasty/hwi-api/internal/ops"
)

func opsGet(h http.Handler, path, peer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = peer
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// getWithin runs opsGet in the background and fails the test if it has not
// returned within the deadline: a request that should be answered at once
// must not be left waiting behind the held ones.
func getWithin(t *testing.T, h http.Handler, path, peer string) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- opsGet(h, path, peer) }()
	select {
	case w := <-done:
		return w
	case <-time.After(5 * time.Second):
		t.Fatalf("GET %s from %s was not answered promptly", path, peer)
		return nil
	}
}

// TestOpsListenerCapsWorkButNotLiveness checks that
// the ops chain turns away work beyond opsMaxConcurrentRequests promptly,
// while /healthz, which bypasses the limiter, still answers 200 for an
// allowed peer and 403 for a denied one when the limiter is full.
func TestOpsListenerCapsWorkButNotLiveness(t *testing.T) {
	agent := metrics.New(buildinfo.Info{}, inventory.NewStore(nil), fstest.MapFS{}, time.Now)
	allow, err := access.New([]string{"127.0.0.1/32"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{}, opsMaxConcurrentRequests+1)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET "+ops.HealthzPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := opsHandler(mux, slog.New(slog.NewTextHandler(io.Discard, nil)), agent, allow)

	const allowed, denied = "127.0.0.1:40000", "10.0.0.1:40000"

	var held sync.WaitGroup
	for range opsMaxConcurrentRequests {
		held.Go(func() { opsGet(h, "/metrics", allowed) })
	}
	for range opsMaxConcurrentRequests {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the in-limit /metrics requests never all reached the handler")
		}
	}

	w := getWithin(t, h, "/metrics", allowed)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/metrics request %d: status = %d, want 503", opsMaxConcurrentRequests+1, w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("the rejected /metrics request carries no Retry-After header")
	}
	select {
	case <-entered:
		t.Errorf("/metrics request %d reached the handler; the ops listener has no concurrency cap", opsMaxConcurrentRequests+1)
	default:
	}

	if w := getWithin(t, h, ops.HealthzPath, allowed); w.Code != http.StatusOK {
		t.Errorf("allowed %s with the limiter full: status = %d, want 200", ops.HealthzPath, w.Code)
	}
	if w := getWithin(t, h, ops.HealthzPath, denied); w.Code != http.StatusForbidden {
		t.Errorf("denied %s with the limiter full: status = %d, want 403", ops.HealthzPath, w.Code)
	}

	close(release)
	held.Wait()
	if w := getWithin(t, h, "/metrics", allowed); w.Code != http.StatusOK {
		t.Errorf("/metrics after the held requests finished: status = %d, want 200", w.Code)
	}
}
