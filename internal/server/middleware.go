package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"strconv"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/access"
	"github.com/krisiasty/hwi-api/internal/auth"
	"github.com/krisiasty/hwi-api/internal/httperr"
	"github.com/krisiasty/hwi-api/internal/metrics"
)

// Middleware wraps an http.Handler with additional behaviour.
type Middleware func(http.Handler) http.Handler

// Chain applies mw to h so that the first argument is outermost: the
// request is served through mw[0] first, then mw[1], and so on, reaching h
// last. This makes the call site read in the order requests actually flow
// through it, e.g.
//
//	Chain(handler, Recover(log), Observe(...), AccessControl(...), Authenticate(...), LimitConcurrency(...))
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for _, m := range slices.Backward(mw) {
		h = m(h)
	}
	return h
}

// The fixed set of reasons a request may be rejected before reaching a
// handler, recorded on metrics.Agent.Rejections. Callers outside this
// package must not invent additional reason strings: an unbounded set of
// reasons is as much a cardinality hazard on this metric as an unbounded
// set of endpoints is on hwi_api_http_requests_total.
//
// AccessControl reports every denied peer as ReasonAllowlist, including one
// an explicit deny entry matched: access.Matcher.Allowed folds
// deny-wins-over-allow into a single bool before it reaches this package.
const (
	ReasonAllowlist        = "allowlist"
	ReasonMTLS             = "mtls"
	ReasonToken            = "token"
	ReasonNoCredentials    = "no_credentials"
	ReasonConcurrencyLimit = "concurrency_limit"
)

// StatusClientClosedRequest is what Observe records, in the requests metric
// and the request log line, for a request whose handler wrote nothing
// because the request's context had ended, typically because the client
// disconnected. It follows nginx's non-standard 499 convention. It is never
// sent to anyone: there is no client left to receive it.
const StatusClientClosedRequest = 499

// requestState is created once per request by Observe and threaded through
// the rest of the chain as a pointer stored in the request context, under
// the unexported requestStateKey. Authenticate mutates authMethod once it
// has admitted the request; Observe reads it back, in its deferred
// post-processing, to log which mechanism admitted the request.
//
// This works only because every middleware below Observe forwards the
// exact same *http.Request pointer down the chain, rather than
// substituting a copy: net/http's ServeMux sets Request.Pattern by
// mutating, in place, the *http.Request it is handed
// (net/http/server.go's ServeMux.ServeHTTP does
// "h, r.Pattern, r.pat, r.matches = mux.findHandler(r)" before calling
// h.ServeHTTP(w, r) with that same r) - so Observe can only see that
// mutation, or this state pointer, if the identity of the *http.Request it
// passed to next survives, unbroken, all the way down to the mux. If
// Authenticate called next.ServeHTTP(w, r.WithContext(...)), it would pass
// a shallow copy: the mux would then set Pattern on that copy, not on the
// pointer Observe still holds, so Observe would always see an empty Pattern
// and a "none" auth method. Authenticate, AccessControl and
// LimitConcurrency all forward r unchanged; if a future middleware is
// inserted below Observe, it must do the same - call next.ServeHTTP(w, r)
// with the request it was given, never r.WithContext, r.Clone or similar -
// or it will break exactly this.
type requestState struct {
	authMethod string
}

type requestStateKey struct{}

// Recover is outermost in the chain so that a panic anywhere below it -
// including inside Observe, access control, authentication or the
// concurrency limiter, not merely inside the handler - still yields a
// generic 500 and a logged stack, rather than a crashed goroutine and a
// connection the client sees only as dropped.
//
// Recover wraps the raw http.ResponseWriter it is given in a recoverWriter
// before passing it down, so that if the wrapped handler panics after
// already writing (or beginning to write) a response, the panic handler
// below can tell and avoid writing a second one: doing so unconditionally
// produces net/http's "superfluous response.WriteHeader call" and appends
// the JSON error object onto whatever was already sent, corrupting an
// already-truncated response rather than replacing it.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &recoverWriter{ResponseWriter: w}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				log.Error("panic recovered",
					"error", fmt.Sprint(rec),
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
				)
				if rw.written {
					// A response was already written before the panic; the
					// client has already received a (now truncated) body
					// or status. Writing again would only corrupt it
					// further, so the panic is logged above and nothing
					// more is written here.
					return
				}
				// The panic value is never written to the response: it may
				// carry sensitive detail the client has no business seeing.
				httperr.Write(w, http.StatusInternalServerError, apiv1.CodeInternal, "internal server error")
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// recoverWriter wraps the raw http.ResponseWriter Recover receives so its
// panic handler can tell whether a write or WriteHeader already reached the
// client before the panic. It follows the same wrap-and-Unwrap pattern as
// statusWriter below: embedding http.ResponseWriter and implementing
// Unwrap() lets http.ResponseController (and any http.Flusher/http.Hijacker
// type assertion) see through this wrapper to the real ResponseWriter
// underneath, which is what every middleware below Recover - Observe's
// statusWriter included - is themselves layered on top of.
type recoverWriter struct {
	http.ResponseWriter
	written bool
}

func (w *recoverWriter) WriteHeader(status int) {
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *recoverWriter) Write(p []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController (and anything else that type-asserts
// for http.Flusher, http.Hijacker, and similar) see through this wrapper to
// the underlying ResponseWriter, per the net/http convention for wrapping
// writers.
func (w *recoverWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusWriter wraps an http.ResponseWriter to capture the status code and
// byte count Observe needs to log and record, without changing how the
// wrapped ResponseWriter behaves.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController (and anything else that type-asserts
// for http.Flusher, http.Hijacker, and similar) see through this wrapper to
// the underlying ResponseWriter, per the net/http convention for wrapping
// writers.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// standardMethods is the fixed, bounded set of HTTP methods recorded
// verbatim on a metric label. HTTP permits any token as an extension
// method, and Observe sits above access control and authentication, so
// without this clamp any peer that can merely reach a listener - not one
// that has passed any check - could allocate an arbitrarily large number
// of never-evicted series on hwi_api_http_requests_total and
// hwi_api_http_request_duration_seconds simply by varying its request
// method. This is the same unbounded-cardinality hazard the endpoint
// label's "unmatched" fallback guards against, applied to method.
var standardMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodConnect: true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// metricMethod clamps m to the nine standard HTTP methods, mapping
// anything else - including any client-chosen extension token - to
// "other", mirroring the endpoint label's "unmatched" fallback.
func metricMethod(m string) string {
	if standardMethods[m] {
		return m
	}
	return "other"
}

// Observe sits above access control so that rejected requests are still
// counted and logged: otherwise an attacker's traffic, which is exactly
// the traffic an operator most needs visibility into during an incident,
// would never appear in either the metrics or the logs. It records
// hwi_api_http_requests_total and hwi_api_http_request_duration_seconds
// on m (tolerating a nil m, since unit tests exercise other middleware
// without a metrics.Agent) and logs one line per request. The endpoint
// label and logged route always come from r.Pattern - the matched route
// pattern - never the raw request path - and the method metric label is
// clamped by metricMethod, so a client cannot grow either series set
// without bound by requesting arbitrary paths or methods. The logged
// method field deliberately keeps the raw token instead: a log line is
// per request, not a retained series the way a Prometheus label is, so it
// carries none of metricMethod's cardinality risk, and knowing a peer
// actually sent, say, PURGE rather than merely "something non-standard" is
// exactly what an incident investigation wants.
//
// Observe allocates the one *http.Request copy this chain makes (via
// r.WithContext, to attach a *requestState pointer) and passes that copy,
// not the original r, to next; see requestState's doc comment for why
// every middleware below Observe must in turn forward that same pointer
// unchanged. Because that copy is the request the mux (eventually) sees,
// r.Pattern is populated on it by the time next.ServeHTTP returns.
//
// The metrics/log recording runs in a defer, with a completed flag set
// only after next.ServeHTTP returns normally, so a panic - which unwinds
// past this defer without ever reaching that line - is still counted, as
// status 500: Recover sits outside Observe and writes its 500 to the raw
// ResponseWriter, not through statusWriter, so sw.status would otherwise
// stay 0 for a request that in fact failed. Observe deliberately does not
// call recover() itself: the panic must continue unwinding to Recover.
func Observe(listener string, m *metrics.Agent, log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := newRequestID()
			sw := &statusWriter{ResponseWriter: w}
			state := &requestState{}
			r = r.WithContext(context.WithValue(r.Context(), requestStateKey{}, state))

			completed := false
			defer func() {
				dur := time.Since(start)

				status := sw.status
				switch {
				case !completed:
					// next.ServeHTTP never returned: a panic is unwinding
					// through this defer on its way to Recover, which will
					// write 500 to the raw ResponseWriter - a write
					// statusWriter never sees.
					status = http.StatusInternalServerError
				case status == 0 && r.Context().Err() != nil:
					// The handler wrote nothing because the client had
					// already gone; recording that as 200 would count an
					// abandoned request as a success.
					status = StatusClientClosedRequest
				case status == 0:
					status = http.StatusOK
				}

				endpoint := r.Pattern
				if endpoint == "" {
					endpoint = "unmatched"
				}
				method := metricMethod(r.Method)

				if m != nil {
					m.ObserveRequest(listener, endpoint, method, strconv.Itoa(status), dur)
				}

				authMethod := state.authMethod
				if authMethod == "" {
					authMethod = "none"
				}
				if log != nil {
					log.Info("request",
						"request_id", id,
						"listener", listener,
						"peer", r.RemoteAddr,
						"method", r.Method,
						"route", endpoint,
						"status", status,
						"duration_ms", dur.Seconds()*1000,
						"bytes", sw.bytes,
						"auth_method", authMethod,
					)
				}
			}()

			next.ServeHTTP(sw, r)
			completed = true
		})
	}
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AccessControl parses the peer from r.RemoteAddr - never from a
// forwarded-for style header, which a client controls completely - and
// admits the request only if matcher allows that address. Rejections
// always report ReasonAllowlist: access.Matcher.Allowed folds its deny
// list check in before returning, so this middleware cannot tell an
// explicit deny from an address simply missing from the allow list.
func AccessControl(listener string, matcher *access.Matcher, m *metrics.Agent) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addrPort, err := netip.ParseAddrPort(r.RemoteAddr)
			if err != nil || !matcher.Allowed(addrPort.Addr()) {
				if m != nil {
					m.CountRejection(listener, ReasonAllowlist)
				}
				httperr.Write(w, http.StatusForbidden, apiv1.CodeForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Authenticate admits a request that either configured mechanism accepts,
// recording the admitting auth.Method into the *requestState Observe
// placed in the request context, for Observe to log once next.ServeHTTP
// returns. auth.ErrNoCredentials (nothing was presented at all) maps to
// 401; any other error (something was presented and refused) maps to 403.
// In neither case does the response say which mechanism failed - that is
// exactly the distinction auth.Authenticator's own contract keeps private
// - so the reason recorded on m is derived independently, from what the
// request itself carried, not from the authenticator's error.
//
// Deliberately calls next.ServeHTTP(w, r) with the exact r it was given,
// never a copy: see requestState's doc comment for why every middleware
// below Observe must preserve that request pointer's identity all the way
// to the mux.
func Authenticate(listener string, a *auth.Authenticator, m *metrics.Agent) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, err := a.Authenticate(r)
			if err != nil {
				status := http.StatusForbidden
				code := apiv1.CodeForbidden
				message := "request not authorized"
				reason := rejectionReason(r)
				if errors.Is(err, auth.ErrNoCredentials) {
					status = http.StatusUnauthorized
					code = apiv1.CodeUnauthorized
					message = "authentication required"
					reason = ReasonNoCredentials
				}
				if m != nil {
					m.CountRejection(listener, reason)
				}
				httperr.Write(w, status, code, message)
				return
			}
			if state, ok := r.Context().Value(requestStateKey{}).(*requestState); ok {
				state.authMethod = string(method)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// rejectionReason reports which mechanism a rejected request attempted, for
// the Rejections metric only - never for the response body. It mirrors,
// from the outside, exactly what auth.Authenticator.Authenticate itself
// treats as "credentials were presented" for each mechanism: a verified TLS
// client certificate chain, or a Bearer credential in the Authorization
// header.
func rejectionReason(r *http.Request) string {
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
		return ReasonMTLS
	}
	return ReasonToken
}

// LimitConcurrency admits at most n requests at a time through next,
// rejecting anything beyond that cap with 503 and Retry-After rather than
// queuing it indefinitely. The semaphore is created once, when
// LimitConcurrency is called, and shared by every request the returned
// Middleware ever wraps.
func LimitConcurrency(listener string, n int, m *metrics.Agent) Middleware {
	sem := make(chan struct{}, n)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				next.ServeHTTP(w, r)
			default:
				if m != nil {
					m.CountRejection(listener, ReasonConcurrencyLimit)
				}
				w.Header().Set("Retry-After", "1")
				httperr.Write(w, http.StatusServiceUnavailable, apiv1.CodeTooManyRequests, "too many concurrent requests")
			}
		})
	}
}
