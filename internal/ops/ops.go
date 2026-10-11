// Package ops serves the unauthenticated operational endpoints.
package ops

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/krisiasty/hwi-api/internal/buildinfo"
)

// Status is the /status payload. It describes the agent, never the host:
// this listener is unauthenticated, so host identity, serials and UUIDs
// belong on the API listener's inventory endpoints instead.
type Status struct {
	Name          string  `json:"name"`
	Version       string  `json:"version"`
	Commit        string  `json:"commit"`
	BuildDate     string  `json:"build_date,omitempty"`
	GoVersion     string  `json:"go_version"`
	Status        string  `json:"status"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}

// Ops serves the operational endpoints.
type Ops struct {
	metrics io.WriterTo
	build   buildinfo.Info
	ready   func() bool
	started time.Time
}

// HealthzPath is the liveness endpoint's path. It is exported so that the
// ops listener's middleware can route it around the concurrency limiter by
// the same path Routes registers it under.
const HealthzPath = "/healthz"

// New builds the operational handlers. /metrics serves what metrics
// writes.
func New(metrics io.WriterTo, build buildinfo.Info, ready func() bool, started time.Time) *Ops {
	return &Ops{metrics: metrics, build: build, ready: ready, started: started}
}

// Routes registers the operational endpoints.
func (o *Ops) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+HealthzPath, o.handleHealthz)
	mux.HandleFunc("GET /readyz", o.handleReadyz)
	mux.HandleFunc("GET /metrics", o.handleMetrics)
	mux.HandleFunc("GET /status", o.handleStatus)
}

// handleHealthz answers liveness: the process is serving. It deliberately
// ignores readiness, so a slow first collection does not get the agent
// restarted by a supervisor.
func (o *Ops) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (o *Ops) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !o.ready() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

func (o *Ops) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	buf := new(bytes.Buffer)
	if _, err := o.metrics.WriteTo(buf); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("error\n"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (o *Ops) handleStatus(w http.ResponseWriter, _ *http.Request) {
	state := "starting"
	if o.ready() {
		state = "ready"
	}
	body := Status{
		Name:          o.build.Name,
		Version:       o.build.Version,
		Commit:        o.build.Commit,
		BuildDate:     o.build.Date,
		GoVersion:     o.build.GoVersion,
		Status:        state,
		UptimeSeconds: time.Since(o.started).Seconds(),
	}
	buf := new(bytes.Buffer)
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("error\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
