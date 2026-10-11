// Package api serves the inventory over HTTP.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/httperr"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

// API serves the inventory endpoints.
type API struct {
	store   *inventory.Store
	hostID  string
	refresh func(ctx context.Context, s apiv1.Subsystem)
}

// New builds the API. hostID is reported as every envelope's host_id; see
// apiv1.Envelope.HostID. refresh forces a synchronous collection for
// ?refresh=true.
func New(store *inventory.Store, hostID string, refresh func(ctx context.Context, s apiv1.Subsystem)) *API {
	return &API{store: store, hostID: hostID, refresh: refresh}
}

// Routes registers the inventory endpoints. Only GET is registered, so any
// other method falls through to the 405 handler below.
func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/inventory", a.handleSnapshot)
	mux.HandleFunc("GET /api/v1/inventory/{subsystem}", a.handleSubsystem)
	mux.HandleFunc("/api/v1/inventory", methodNotAllowed)
	mux.HandleFunc("/api/v1/inventory/{subsystem}", methodNotAllowed)
}

func (a *API) handleSubsystem(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("subsystem")
	sub, ok := apiv1.ParseSubsystem(name)
	if !ok {
		httperr.Write(w, http.StatusNotFound, apiv1.CodeNotFound, "unknown subsystem")
		return
	}
	for _, d := range a.store.Disabled() {
		if d == sub {
			httperr.Write(w, http.StatusNotFound, apiv1.CodeSubsystemDisabled,
				"subsystem is disabled in configuration")
			return
		}
	}

	if wantsRefresh(r) && a.refresh != nil {
		a.refresh(r.Context(), sub)
		// The client may have gone while the probe ran; there is then no
		// one to build a response for. See canceled.
		if canceled(r) {
			return
		}
	}

	entry, ok := a.store.Get(sub)
	if !ok || len(entry.Body) == 0 {
		httperr.Write(w, http.StatusServiceUnavailable, apiv1.CodeSubsystemUnavailable,
			"subsystem has not been collected")
		return
	}

	env := a.envelope(sub, entry.CollectedAt, true)
	env.Data = entry.Body
	if entry.Err != "" {
		// Data is present but the refresh is failing: serve the last-good
		// payload and say so. Complete stays true because data exists.
		env.Errors = map[apiv1.Subsystem]string{sub: entry.Err}
	}
	writeEnvelope(w, r, env, a.etag(string(sub), entry.Digest), entry.CollectedAt)
}

func (a *API) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	disabled := a.store.Disabled()

	if wantsRefresh(r) && a.refresh != nil {
		for _, s := range apiv1.Subsystems() {
			if isDisabled(disabled, s) {
				// An operator who disabled a subsystem to avoid its probe
				// cost must not have that cost forced by a snapshot
				// refresh; handleSubsystem already refuses this per-subsystem.
				continue
			}
			if canceled(r) {
				return
			}
			a.refresh(r.Context(), s)
		}
		if canceled(r) {
			return
		}
	}

	entries := a.store.Snapshot()
	data := make(map[apiv1.Subsystem]apiv1.SnapshotEntry, len(entries))
	errs := map[apiv1.Subsystem]string{}

	// Every entry's digest and error go into the ETag. collected_at does
	// not; see etag. Whether each enabled subsystem is present, and so
	// complete and the "not collected" errors below, follows from this
	// same entry list, and disabled is fixed configuration.
	tagParts := make([]string, 0, 3*len(entries))

	var oldest time.Time
	present := 0
	for _, e := range entries {
		tagParts = append(tagParts, string(e.Subsystem), e.Digest, e.Err)
		if e.Err != "" {
			errs[e.Subsystem] = e.Err
		}
		if len(e.Body) == 0 {
			continue
		}
		present++
		data[e.Subsystem] = apiv1.SnapshotEntry{CollectedAt: e.CollectedAt.UTC(), Data: e.Body}
		if oldest.IsZero() || e.CollectedAt.Before(oldest) {
			oldest = e.CollectedAt
		}
	}

	// 503 means "the service can't answer right now". That is true only
	// when some subsystem is enabled and expected to have data but does
	// not (the cold-start case, gated by /readyz until the first refresh
	// cycle completes). When every subsystem is disabled in configuration,
	// nothing has failed: the host is working exactly as configured, and a
	// fully-disabled host is a legitimate steady state that must not 503
	// its inventory endpoint forever.
	enabledCount := len(apiv1.Subsystems()) - len(disabled)
	if present == 0 && enabledCount > 0 {
		httperr.Write(w, http.StatusServiceUnavailable, apiv1.CodeSubsystemUnavailable,
			"no subsystem has been collected")
		return
	}

	// Complete means every enabled subsystem has data. Disabled subsystems
	// are intentionally absent and do not count against it. When there are
	// no enabled subsystems at all, this is vacuously true.
	complete := true
	for _, s := range apiv1.Subsystems() {
		if isDisabled(disabled, s) {
			continue
		}
		if _, ok := data[s]; !ok {
			complete = false
			if _, named := errs[s]; !named {
				errs[s] = "not collected"
			}
		}
	}

	env := a.envelope(apiv1.SubsystemAll, oldest, complete)
	env.Disabled = disabled
	if len(errs) > 0 {
		env.Errors = errs
	}
	body, err := json.Marshal(data)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, apiv1.CodeInternal, "encoding snapshot")
		return
	}
	env.Data = body

	writeEnvelope(w, r, env, a.etag(tagParts...), oldest)
}

func (a *API) envelope(sub apiv1.Subsystem, collectedAt time.Time, complete bool) apiv1.Envelope {
	return apiv1.Envelope{
		SchemaVersion: apiv1.SchemaVersion,
		Subsystem:     sub,
		HostID:        a.hostID,
		CollectedAt:   collectedAt.UTC(),
		Complete:      complete,
	}
}

func writeEnvelope(w http.ResponseWriter, r *http.Request, env apiv1.Envelope, etag string, modified time.Time) {
	body, err := json.Marshal(env)
	if err != nil {
		httperr.Write(w, http.StatusInternalServerError, apiv1.CodeInternal, "encoding response")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("ETag", etag)
	if !modified.IsZero() {
		h.Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}

	if matches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "GET, HEAD")
	httperr.Write(w, http.StatusMethodNotAllowed, apiv1.CodeMethodNotAllowed,
		"only GET and HEAD are supported")
}

// canceled reports whether r's context has ended, typically because the
// client disconnected, so that a handler can stop instead of refreshing
// further subsystems or building a response nobody will read. It is checked
// between and after forced refreshes. A probe that has already started
// cannot be interrupted, because the probers do not watch ctx: it runs to
// completion, and its result is stored and scheduled as usual, since it is
// valid inventory. Only the work after it is skipped. The handler then
// writes nothing, and server.Observe records the request as
// StatusClientClosedRequest.
func canceled(r *http.Request) bool {
	return r.Context().Err() != nil
}

func wantsRefresh(r *http.Request) bool {
	return r.URL.Query().Get("refresh") == "true"
}

func isDisabled(disabled []apiv1.Subsystem, s apiv1.Subsystem) bool {
	for _, d := range disabled {
		if d == s {
			return true
		}
	}
	return false
}

// matches implements the If-None-Match comparison, which is a list and may
// be the wildcard. RFC 9110 section 13.1.2 makes it a weak comparison: two
// tags match when their opaque parts are equal, whether or not either
// carries the W/ prefix.
func matches(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == want {
			return true
		}
	}
	return false
}

// etag builds a weak validator from parts (store digests, error messages)
// plus the fixed envelope fields a client would also see change: the
// schema version and host_id.
//
// It is weak, not strong, because collected_at is deliberately left out.
// A refresh that re-collects identical hardware moves collected_at, and
// with the default one-minute interval a validator that covered it would
// change on every host every minute, so a sync across many hosts would
// never get a 304. What the validator promises instead is the weak one: the same
// hardware and the same error state, which is what a consumer revalidates
// for.
//
// Each part is length-prefixed so that no two different part lists hash
// the same bytes.
func (a *API) etag(parts ...string) string {
	h := sha256.New()
	fixed := []string{apiv1.SchemaVersion, a.hostID}
	for _, p := range append(fixed, parts...) {
		_, _ = fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return `W/"` + hex.EncodeToString(h.Sum(nil)) + `"`
}
