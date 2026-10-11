// Package metrics renders the agent's Prometheus text exposition: the
// agent's own request, probe and readiness series, a few Go runtime and
// process metrics, and the age of each subsystem's inventory data.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/buildinfo"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

// latencyBuckets covers sub-millisecond to ten-second HTTP request
// durations.
var latencyBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// probeBuckets covers probe durations up to half a minute: cheap probes
// (cpu, memory) finish in milliseconds, but firmware and PCI enumeration
// can take seconds under load.
var probeBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Agent records the agent's own metrics and renders the whole exposition.
// Every series is keyed by its rendered label block, whose label names each
// recording method writes in alphabetical order.
type Agent struct {
	buildLabels string
	store       *inventory.Store
	fsys        fs.FS
	now         func() time.Time

	mu              sync.Mutex
	requests        map[string]uint64
	requestDuration histogram
	rejections      map[string]uint64
	probeDuration   histogram
	probeErrors     map[string]uint64
	refreshes       map[string]uint64
	ready           bool
}

// histogram holds one family's series, all sharing the same bucket bounds.
type histogram struct {
	bounds []float64
	series map[string]*histogramSeries
}

// histogramSeries has one count per bound plus a final overflow count for
// values above every bound.
type histogramSeries struct {
	counts []uint64
	sum    float64
	last   float64
}

func (h histogram) observe(labels string, v float64) {
	s, ok := h.series[labels]
	if !ok {
		s = &histogramSeries{counts: make([]uint64, len(h.bounds)+1)}
		h.series[labels] = s
	}
	s.counts[sort.SearchFloat64s(h.bounds, v)]++
	s.sum += v
	s.last = v
}

// New returns an Agent reporting b as hwi_api_build_info, the age of
// store's data as of now(), and process metrics read from fsys, which is
// expected to behave like the host root: /proc files are read from it by
// relative path, and any that cannot be read are left out of the scrape.
func New(b buildinfo.Info, store *inventory.Store, fsys fs.FS, now func() time.Time) *Agent {
	return &Agent{
		buildLabels:     labels("commit", b.Commit, "goversion", b.GoVersion, "version", b.Version),
		store:           store,
		fsys:            fsys,
		now:             now,
		requests:        map[string]uint64{},
		requestDuration: histogram{bounds: latencyBuckets, series: map[string]*histogramSeries{}},
		rejections:      map[string]uint64{},
		probeDuration:   histogram{bounds: probeBuckets, series: map[string]*histogramSeries{}},
		probeErrors:     map[string]uint64{},
		refreshes:       map[string]uint64{},
	}
}

// var _ inventory.Recorder = (*Agent)(nil) asserts, at compile time, that
// an *Agent can be passed straight to inventory.NewRefresher.
var _ inventory.Recorder = (*Agent)(nil)

// ObserveProbe records one subsystem probe's outcome: its duration always,
// a refresh attempt labeled by result, and a probe error when err is
// non-nil.
func (a *Agent) ObserveProbe(sub apiv1.Subsystem, d time.Duration, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.probeDuration.observe(labels("subsystem", string(sub)), d.Seconds())
	if err != nil {
		a.probeErrors[labels("subsystem", string(sub))]++
	}
	a.refreshes[labels("result", result, "subsystem", string(sub))]++
}

// ObserveRequest records one served HTTP request. endpoint must be a route
// pattern (e.g. "/api/v1/inventory/{subsystem}"), never the raw request
// path, or a client can drive unbounded label cardinality.
func (a *Agent) ObserveRequest(listener, endpoint, method, code string, d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests[labels("code", code, "endpoint", endpoint, "listener", listener, "method", method)]++
	a.requestDuration.observe(labels("endpoint", endpoint, "listener", listener, "method", method), d.Seconds())
}

// CountRejection records one request turned away before reaching a
// handler.
func (a *Agent) CountRejection(listener, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rejections[labels("listener", listener, "reason", reason)]++
}

// SetReady sets hwi_api_ready to 1 when ready is true, and to 0
// otherwise.
func (a *Agent) SetReady(ready bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready = ready
}

// WriteTo renders the exposition and writes it to w in a single Write:
// the agent's own families in name order, then the runtime, process and
// inventory metrics, each read fresh.
func (a *Agent) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	a.writeAgent(&buf)
	writeRuntime(&buf)
	writeProcess(&buf, a.fsys)
	writeInventory(&buf, a.store, a.now())
	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

func (a *Agent) writeAgent(buf *bytes.Buffer) {
	a.mu.Lock()
	defer a.mu.Unlock()

	writeHeader(buf, "hwi_api_build_info", "gauge", "Build information about the running binary, always 1.")
	fmt.Fprintf(buf, "hwi_api_build_info%s 1\n", a.buildLabels)
	writeHistogram(buf, "hwi_api_http_request_duration_seconds", "HTTP request latency in seconds, by listener, endpoint and method.", a.requestDuration)
	writeLastHistogramGauge(buf, "hwi_api_http_request_last_duration_seconds", "Duration in seconds of the most recently completed HTTP request, by listener, endpoint and method.", a.requestDuration)
	writeCounter(buf, "hwi_api_http_requests_total", "HTTP requests served, by listener, endpoint, method and status code.", a.requests)
	writeHistogram(buf, "hwi_api_probe_duration_seconds", "Subsystem probe duration in seconds.", a.probeDuration)
	writeCounter(buf, "hwi_api_probe_errors_total", "Subsystem probe failures.", a.probeErrors)
	writeLastHistogramGauge(buf, "hwi_api_probe_last_duration_seconds", "Duration in seconds of the most recently completed subsystem probe, including failed probes.", a.probeDuration)
	ready := 0
	if a.ready {
		ready = 1
	}
	writeHeader(buf, "hwi_api_ready", "gauge", "Whether the agent has completed its first refresh cycle.")
	fmt.Fprintf(buf, "hwi_api_ready %d\n", ready)
	writeCounter(buf, "hwi_api_refreshes_total", "Subsystem refresh attempts, by outcome.", a.refreshes)
	writeCounter(buf, "hwi_api_request_rejections_total", "Requests rejected before reaching a handler, by listener and reason.", a.rejections)
}

// writeInventory writes each subsystem's data age and last successful
// probe time, in subsystem name order. A subsystem quietly serving
// hours-old last-good data looks healthy by status code alone; age is the
// signal that exposes it. One that has never collected successfully (a
// zero CollectedAt) is left out rather than reported as implausibly old.
func writeInventory(buf *bytes.Buffer, store *inventory.Store, now time.Time) {
	entries := store.Snapshot()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Subsystem < entries[j].Subsystem })

	writeHeader(buf, "hwi_api_data_age_seconds", "gauge", "Age in seconds of each subsystem's most recently collected data.")
	for _, e := range entries {
		if !e.CollectedAt.IsZero() {
			fmt.Fprintf(buf, "hwi_api_data_age_seconds%s %s\n", labels("subsystem", string(e.Subsystem)), formatFloat(now.Sub(e.CollectedAt).Seconds()))
		}
	}
	writeHeader(buf, "hwi_api_probe_last_success_timestamp_seconds", "gauge", "Unix timestamp of each subsystem's last successful probe.")
	for _, e := range entries {
		if !e.CollectedAt.IsZero() {
			fmt.Fprintf(buf, "hwi_api_probe_last_success_timestamp_seconds%s %s\n", labels("subsystem", string(e.Subsystem)), formatFloat(float64(e.CollectedAt.Unix())))
		}
	}
}

func writeHeader(buf *bytes.Buffer, name, typ, help string) {
	fmt.Fprintf(buf, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeCounter(buf *bytes.Buffer, name, help string, series map[string]uint64) {
	writeHeader(buf, name, "counter", help)
	for _, l := range slices.Sorted(maps.Keys(series)) {
		fmt.Fprintf(buf, "%s%s %d\n", name, l, series[l])
	}
}

func writeHistogram(buf *bytes.Buffer, name, help string, h histogram) {
	writeHeader(buf, name, "histogram", help)
	for _, l := range slices.Sorted(maps.Keys(h.series)) {
		s := h.series[l]
		// Every label block is "{...}", so le is appended inside the braces.
		open := l[:len(l)-1] + ","
		var cumulative uint64
		for i, bound := range h.bounds {
			cumulative += s.counts[i]
			fmt.Fprintf(buf, "%s_bucket%sle=\"%s\"} %d\n", name, open, formatFloat(bound), cumulative)
		}
		cumulative += s.counts[len(h.bounds)]
		fmt.Fprintf(buf, "%s_bucket%sle=\"+Inf\"} %d\n", name, open, cumulative)
		fmt.Fprintf(buf, "%s_sum%s %s\n", name, l, formatFloat(s.sum))
		fmt.Fprintf(buf, "%s_count%s %d\n", name, l, cumulative)
	}
}

func writeLastHistogramGauge(buf *bytes.Buffer, name, help string, h histogram) {
	writeHeader(buf, name, "gauge", help)
	for _, l := range slices.Sorted(maps.Keys(h.series)) {
		fmt.Fprintf(buf, "%s%s %s\n", name, l, formatFloat(h.series[l].last))
	}
}

// labels renders alternating names and values as a label block, in the
// order given: callers pass the names in alphabetical order.
func labels(pairs ...string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pairs[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(pairs[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabelValue escapes a label value as the text format requires:
// backslash first, so the escapes added after it are not doubled.
func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return strings.ReplaceAll(v, "\n", `\n`)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
