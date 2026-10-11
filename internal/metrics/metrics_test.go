package metrics

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/buildinfo"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

var t0 = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func newTestAgent(store *inventory.Store) *Agent {
	if store == nil {
		store = inventory.NewStore(nil)
	}
	return New(buildinfo.Info{Version: "1.2.3", Commit: "abc1234", GoVersion: "go1.27.1"}, store, fstest.MapFS{}, func() time.Time { return t0 })
}

// render returns the agent's own families and the inventory section: the
// whole exposition except the runtime and process metrics, whose values
// depend on the environment.
func render(a *Agent) string {
	var buf bytes.Buffer
	a.writeAgent(&buf)
	writeInventory(&buf, a.store, a.now())
	return buf.String()
}

func mustContain(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("exposition is missing %q:\n%s", w, got)
		}
	}
}

func TestBuildInfo(t *testing.T) {
	mustContain(t, render(newTestAgent(nil)), `hwi_api_build_info{commit="abc1234",goversion="go1.27.1",version="1.2.3"} 1`)
}

func TestObserveProbe(t *testing.T) {
	a := newTestAgent(nil)
	a.ObserveProbe(apiv1.SubsystemCPU, 0, nil)
	a.ObserveProbe(apiv1.SubsystemCPU, 0, errors.New("boom"))

	mustContain(t, render(a),
		`hwi_api_refreshes_total{result="success",subsystem="cpu"} 1`,
		`hwi_api_refreshes_total{result="error",subsystem="cpu"} 1`,
		`hwi_api_probe_errors_total{subsystem="cpu"} 1`,
		`hwi_api_probe_duration_seconds_count{subsystem="cpu"} 2`,
	)
}

func TestLastDurationGauges(t *testing.T) {
	a := newTestAgent(nil)
	a.ObserveProbe(apiv1.SubsystemCPU, 3*time.Millisecond, nil)
	a.ObserveProbe(apiv1.SubsystemCPU, 7*time.Millisecond, errors.New("boom"))
	a.ObserveProbe(apiv1.SubsystemPCI, 11*time.Millisecond, nil)
	a.ObserveRequest("api", "/x", "GET", "200", 2*time.Millisecond)
	a.ObserveRequest("api", "/x", "GET", "500", 4*time.Millisecond)
	a.ObserveRequest("api", "/x", "HEAD", "200", 6*time.Millisecond)

	mustContain(t, render(a),
		"\nhwi_api_probe_last_duration_seconds{subsystem=\"cpu\"} 0.007\n",
		"\nhwi_api_probe_last_duration_seconds{subsystem=\"pci\"} 0.011\n",
		"\nhwi_api_http_request_last_duration_seconds{endpoint=\"/x\",listener=\"api\",method=\"GET\"} 0.004\n",
		"\nhwi_api_http_request_last_duration_seconds{endpoint=\"/x\",listener=\"api\",method=\"HEAD\"} 0.006\n",
	)
}

func TestSetReady(t *testing.T) {
	a := newTestAgent(nil)
	mustContain(t, render(a), "\nhwi_api_ready 0\n")
	a.SetReady(true)
	mustContain(t, render(a), "\nhwi_api_ready 1\n")
	a.SetReady(false)
	mustContain(t, render(a), "\nhwi_api_ready 0\n")
}

// A family with nothing recorded yet still has its HELP and TYPE, so the
// set of families a scrape sees does not depend on traffic.
func TestEmptyFamiliesKeepTheirHeaders(t *testing.T) {
	mustContain(t, render(newTestAgent(nil)),
		"# TYPE hwi_api_http_request_last_duration_seconds gauge\n# HELP hwi_api_http_requests_total",
		"# TYPE hwi_api_http_requests_total counter\n# HELP hwi_api_probe_duration_seconds",
		"# TYPE hwi_api_probe_last_duration_seconds gauge\n# HELP hwi_api_ready",
		"# TYPE hwi_api_request_rejections_total counter\n# HELP hwi_api_data_age_seconds",
	)
}

func TestLabelValuesAreEscaped(t *testing.T) {
	a := newTestAgent(nil)
	a.CountRejection(`back\slash "quoted"`+"\nnext", "busy")
	mustContain(t, render(a), `hwi_api_request_rejections_total{listener="back\\slash \"quoted\"\nnext",reason="busy"} 1`)
}

// Buckets are cumulative and le is inclusive: a value on a bound counts in
// that bucket, and one above every bound only in +Inf.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	a := newTestAgent(nil)
	for _, d := range []time.Duration{time.Millisecond, 7 * time.Millisecond, 10 * time.Millisecond, time.Minute} {
		a.CountRejection("x", "y") // unrelated series, to show families stay apart
		a.ObserveRequest("api", "/x", "GET", "200", d)
	}
	mustContain(t, render(a),
		`hwi_api_http_request_duration_seconds_bucket{endpoint="/x",listener="api",method="GET",le="0.001"} 1`,
		`hwi_api_http_request_duration_seconds_bucket{endpoint="/x",listener="api",method="GET",le="0.005"} 1`,
		`hwi_api_http_request_duration_seconds_bucket{endpoint="/x",listener="api",method="GET",le="0.01"} 3`,
		`hwi_api_http_request_duration_seconds_bucket{endpoint="/x",listener="api",method="GET",le="10"} 3`,
		`hwi_api_http_request_duration_seconds_bucket{endpoint="/x",listener="api",method="GET",le="+Inf"} 4`,
		`hwi_api_http_request_duration_seconds_sum{endpoint="/x",listener="api",method="GET"} 60.018`,
		`hwi_api_http_request_duration_seconds_count{endpoint="/x",listener="api",method="GET"} 4`,
		`hwi_api_request_rejections_total{listener="x",reason="y"} 4`,
	)
}

// Series are sorted by their label block, whatever order they were first
// recorded in.
func TestSeriesAreSorted(t *testing.T) {
	a := newTestAgent(nil)
	for _, sub := range []apiv1.Subsystem{apiv1.SubsystemPCI, apiv1.SubsystemBIOS, apiv1.SubsystemMemory, apiv1.SubsystemCPU} {
		a.ObserveProbe(sub, 0, errors.New("x"))
	}
	mustContain(t, render(a), `hwi_api_probe_errors_total{subsystem="bios"} 1
hwi_api_probe_errors_total{subsystem="cpu"} 1
hwi_api_probe_errors_total{subsystem="memory"} 1
hwi_api_probe_errors_total{subsystem="pci"} 1
`)
}

// A subsystem that has never collected successfully has no age to report.
func TestInventoryLeavesOutNeverCollected(t *testing.T) {
	store := inventory.NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemPCI: true})
	if err := store.Put(apiv1.SubsystemPCI, map[string]int{}, t0.Add(-90*time.Second)); err != nil {
		t.Fatal(err)
	}
	store.Fail(apiv1.SubsystemCPU, errors.New("x"), t0)

	got := render(newTestAgent(store))
	mustContain(t, got,
		`hwi_api_data_age_seconds{subsystem="pci"} 90`,
		`hwi_api_probe_last_success_timestamp_seconds{subsystem="pci"} 1.79033751e+09`,
	)
	if strings.Contains(got, `subsystem="cpu"`) {
		t.Errorf("a subsystem that never collected is reported:\n%s", got)
	}
}

// writeRecorder records how many Write calls it received.
type writeRecorder struct {
	bytes.Buffer
	writes int
}

func (w *writeRecorder) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

// WriteTo renders every section and hands w the whole exposition at once.
func TestWriteToWritesEverythingOnce(t *testing.T) {
	var w writeRecorder
	n, err := newTestAgent(nil).WriteTo(&w)
	if err != nil {
		t.Fatal(err)
	}
	if w.writes != 1 || n != int64(w.Len()) {
		t.Errorf("WriteTo made %d writes and reported %d of %d bytes, want one write of all of them", w.writes, n, w.Len())
	}
	mustContain(t, w.String(), "\nhwi_api_ready 0\n", "\ngo_goroutines ", "\n# TYPE hwi_api_data_age_seconds gauge\n")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestWriteToReturnsTheWriteError(t *testing.T) {
	if _, err := newTestAgent(nil).WriteTo(failingWriter{}); err == nil {
		t.Error("WriteTo to a failing writer returned no error")
	}
}

// Scrapes run concurrently with recording; go test -race catches any
// access outside the lock.
func TestConcurrentWriteToDoesNotRace(t *testing.T) {
	a := newTestAgent(nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, sub := range []apiv1.Subsystem{apiv1.SubsystemCPU, apiv1.SubsystemPCI} {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				a.ObserveProbe(sub, time.Duration(i)*time.Millisecond, nil)
				a.ObserveRequest("api", "/x", "GET", "200", time.Millisecond)
				a.CountRejection("api", "busy")
				a.SetReady(i%2 == 0)
			}
		})
	}
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := a.WriteTo(io.Discard); err != nil {
				t.Errorf("WriteTo: %v", err)
			}
		}
	})
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestExpositionMatchesGolden renders a fixed set of events and compares
// the whole text with a checked-in fixture, first produced by the generic
// registry this package replaced, so the families, series order and
// formatting stayed as they were. Set UPDATE_GOLDEN to rewrite it.
func TestExpositionMatchesGolden(t *testing.T) {
	store := inventory.NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemBIOS: true, apiv1.SubsystemPCI: true})
	if err := store.Put(apiv1.SubsystemPCI, map[string]int{"a": 1}, t0.Add(-90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(apiv1.SubsystemBIOS, map[string]int{"a": 1}, t0.Add(-1500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	store.Fail(apiv1.SubsystemCPU, errors.New("x"), t0)

	a := New(buildinfo.Info{Version: `1.2"3`, Commit: `abc\d`, GoVersion: "go1.27.1"}, store, fstest.MapFS{}, func() time.Time { return t0 })
	a.SetReady(true)
	for i, d := range []time.Duration{0, time.Millisecond, 3 * time.Millisecond, 700 * time.Millisecond, 45 * time.Second, 12 * time.Second} {
		a.ObserveRequest("api", "/api/v1/inventory/{subsystem}", "GET", "200", d)
		a.ObserveRequest("ops", "/metrics", "GET", "200", d/2)
		if i%2 == 0 {
			a.ObserveRequest("api", "/api/v1/inventory/{subsystem}", "HEAD", "304", d)
			a.ObserveRequest("api", "line\nbreak", "GET", "404", d)
		}
		a.ObserveProbe(apiv1.SubsystemPCI, d, nil)
		a.ObserveProbe(apiv1.SubsystemCPU, d*3, errors.New("boom"))
		a.ObserveProbe(apiv1.SubsystemBIOS, d, nil)
	}
	a.CountRejection("api", "forbidden")
	a.CountRejection("api", "unauthorized")
	a.CountRejection("ops", "busy")
	a.CountRejection("api", "forbidden")

	got := render(a)

	const golden = "testdata/exposition.golden"
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	if got != string(want) {
		t.Errorf("exposition does not match golden file:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
