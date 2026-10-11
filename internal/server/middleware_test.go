package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/krisiasty/hwi-api/internal/access"
	"github.com/krisiasty/hwi-api/internal/auth"
	"github.com/krisiasty/hwi-api/internal/buildinfo"
	"github.com/krisiasty/hwi-api/internal/config"
	"github.com/krisiasty/hwi-api/internal/inventory"
	"github.com/krisiasty/hwi-api/internal/metrics"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func callWithPeer(t *testing.T, h http.Handler, peer string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/inventory/cpu", nil)
	r.RemoteAddr = peer
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRecoverTurnsAPanicIntoA500(t *testing.T) {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := Chain(boom, Recover(quiet()))

	w := callWithPeer(t, h, "10.0.0.5:5000")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if body := w.Body.String(); body == "" || !contains(body, "internal") {
		t.Errorf("body = %q, want a generic error object", body)
	}
	if contains(w.Body.String(), "boom") {
		t.Error("the panic value must not reach the client")
	}
}

// TestRecoverDoesNotWriteASecondResponseAfterAPartialWrite guards against
// Recover writing a second response on top of one the wrapped handler had
// already begun: doing so unconditionally produces net/http's "superfluous
// response.WriteHeader call" against a real ResponseWriter and appends the
// JSON error object onto whatever bytes were already sent, corrupting an
// already-truncated body rather than replacing it. Here the handler writes
// a partial body - with no explicit WriteHeader, so the write itself
// implies a 200 - and then panics; the body must come back exactly as
// written, with nothing appended, and the panic must still be logged.
func TestRecoverDoesNotWriteASecondResponseAfterAPartialWrite(t *testing.T) {
	boom := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	})
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, nil))
	h := Chain(boom, Recover(log))

	w := callWithPeer(t, h, "10.0.0.5:5000")

	if got := w.Body.String(); got != "partial" {
		t.Errorf("body = %q, want exactly the partial write with nothing appended", got)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want the 200 implied by the partial write, left untouched", w.Code)
	}
	if !contains(logBuf.String(), "panic recovered") {
		t.Error("the panic must still be logged even though no second response is written")
	}
}

func TestAccessControlAppliesTheMatcher(t *testing.T) {
	m, err := access.New([]string{"10.0.0.0/16"}, []string{"10.0.7.13/32"})
	if err != nil {
		t.Fatal(err)
	}
	h := Chain(ok(), AccessControl("api", m, nil))

	if w := callWithPeer(t, h, "10.0.0.5:5000"); w.Code != http.StatusOK {
		t.Errorf("allowed peer = %d, want 200", w.Code)
	}
	if w := callWithPeer(t, h, "10.0.7.13:5000"); w.Code != http.StatusForbidden {
		t.Errorf("denied peer = %d, want 403", w.Code)
	}
	if w := callWithPeer(t, h, "192.168.1.1:5000"); w.Code != http.StatusForbidden {
		t.Errorf("unlisted peer = %d, want 403", w.Code)
	}
}

func TestAccessControlIgnoresForwardedHeaders(t *testing.T) {
	m, _ := access.New([]string{"10.0.0.0/16"}, nil)
	h := Chain(ok(), AccessControl("api", m, nil))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/inventory/cpu", nil)
	r.RemoteAddr = "192.168.1.1:5000"
	r.Header.Set("X-Forwarded-For", "10.0.0.5")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Error("X-Forwarded-For must never be trusted; the peer address decides")
	}
}

func TestLimitConcurrencyRejectsBeyondTheCap(t *testing.T) {
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)

	blocking := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started.Done()
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := Chain(blocking, LimitConcurrency("api", 1, nil))

	go func() { _ = callWithPeer(t, h, "10.0.0.5:5000") }()
	started.Wait()

	w := callWithPeer(t, h, "10.0.0.6:5000")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 once the limit is reached", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a 503 from the limiter should carry Retry-After")
	}
	close(release)
}

func tokenDigest(t *testing.T, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newObservedAuthenticatedChain builds Observe wrapping Authenticate
// wrapping a real *http.ServeMux with one wildcard-pattern route, so
// r.Pattern is populated the way production code populates it (never
// faked), and Authenticate is deliberately in the chain: it is the
// middleware most likely to break the pointer identity Observe depends
// on. The returned Agent and log buffer let a test inspect
// what Observe recorded.
func newObservedAuthenticatedChain(t *testing.T) (h http.Handler, agent *metrics.Agent, logBuf *bytes.Buffer) {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle("GET /items/{id}", ok())

	agent = newAgent()

	logBuf = &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, nil))

	authr, err := auth.New(config.AuthConfig{Token: config.TokenConfig{
		Enabled: true, Hashes: []string{tokenDigest(t, "s3cr3t")},
	}})
	if err != nil {
		t.Fatal(err)
	}

	h = Chain(mux, Observe("test", agent, log), Authenticate("test", authr, agent))
	return h, agent, logBuf
}

func authenticatedItemRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/items/42", nil)
	r.RemoteAddr = "10.0.0.5:5000"
	r.Header.Set("Authorization", "Bearer s3cr3t")
	return r
}

// TestObserveClampsNonStandardMethodToOther guards against the same
// unbounded-cardinality hazard already fixed for the endpoint label, but
// through r.Method: HTTP allows any token as an extension method, so
// passing it straight into a metric label lets any peer that can reach the
// listener allocate a new, never-evicted series - before access control or
// authentication have had any say, since Observe sits above both.
func TestObserveClampsNonStandardMethodToOther(t *testing.T) {
	agent := newAgent()

	h := Chain(ok(), Observe("test", agent, quiet()))

	r := httptest.NewRequest("PURGE", "/api/v1/inventory/cpu", nil)
	r.RemoteAddr = "10.0.0.5:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var buf bytes.Buffer
	if _, err := agent.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if !contains(buf.String(), `method="other"`) {
		t.Errorf("metrics = %q, want a method=\"other\" series for a non-standard method", buf.String())
	}
	if contains(buf.String(), `method="PURGE"`) {
		t.Error("the raw non-standard method must never become a metric label value")
	}
}

// TestObserveRecordsRoutePatternWithAuthenticateInChain guards against
// Authenticate silently disconnecting Observe from r.Pattern by copying
// the request (net/http.Request.WithContext is a shallow copy, and
// net/http.ServeMux sets Pattern on the exact *http.Request pointer it is
// handed - not on any earlier copy an outer middleware still holds).
func TestObserveRecordsRoutePatternWithAuthenticateInChain(t *testing.T) {
	h, agent, _ := newObservedAuthenticatedChain(t)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authenticatedItemRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	var buf bytes.Buffer
	if _, err := agent.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	// http.ServeMux.Request.Pattern reports the pattern exactly as
	// registered, method prefix included.
	if !contains(buf.String(), `endpoint="GET /items/{id}"`) {
		t.Errorf("metrics = %q, want endpoint \"GET /items/{id}\"", buf.String())
	}
	if contains(buf.String(), `endpoint="unmatched"`) {
		t.Error("a request that matched a mux pattern must not be recorded as \"unmatched\"")
	}
}

// TestObserveLogsTheAdmittingAuthMethod guards the same pointer-identity
// property as the test above, from the log side: the auth method
// Authenticate admits the request under must reach Observe's log line,
// not always read back as "none".
func TestObserveLogsTheAdmittingAuthMethod(t *testing.T) {
	h, _, logBuf := newObservedAuthenticatedChain(t)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authenticatedItemRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	if !contains(logBuf.String(), `"auth_method":"token"`) {
		t.Errorf("log = %q, want auth_method \"token\"", logBuf.String())
	}
}

// TestObserveCountsAPanicAs500 guards against Observe's post-processing
// running only on a normal return: without a defer, a panic unwinds
// straight past it to Recover, so the 500 Recover writes is never counted
// in hwi_api_http_requests_total - exactly the status class an operator
// most needs visibility into during an incident.
func TestObserveCountsAPanicAs500(t *testing.T) {
	agent := newAgent()

	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := Chain(boom, Recover(quiet()), Observe("test", agent, quiet()))

	w := callWithPeer(t, h, "10.0.0.5:5000")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}

	var buf bytes.Buffer
	if _, err := agent.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if !contains(buf.String(), `code="500"`) {
		t.Errorf("metrics = %q, want a code=\"500\" series recorded for the panic", buf.String())
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// newAgent returns an Agent with an empty store and no /proc to read.
func newAgent() *metrics.Agent {
	return metrics.New(buildinfo.Info{}, inventory.NewStore(nil), fstest.MapFS{}, time.Now)
}

// TestObserveRecordsAnAbandonedRequestAs499: a handler that writes nothing
// because the request's context has ended must not be counted as a 200, or
// requests abandoned mid-refresh would look like successes.
func TestObserveRecordsAnAbandonedRequestAs499(t *testing.T) {
	for _, tc := range []struct {
		name     string
		canceled bool
		want     string
	}{
		{"canceled", true, `code="499"`},
		{"not canceled", false, `code="200"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newAgent()
			silent := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
			h := Chain(silent, Observe("test", agent, quiet()))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			r := httptest.NewRequest(http.MethodGet, "/api/v1/inventory", nil).WithContext(ctx)
			h.ServeHTTP(httptest.NewRecorder(), r)

			var buf bytes.Buffer
			if _, err := agent.WriteTo(&buf); err != nil {
				t.Fatal(err)
			}
			if !contains(buf.String(), tc.want) {
				t.Errorf("metrics = %q, want a %s series", buf.String(), tc.want)
			}
		})
	}
}
