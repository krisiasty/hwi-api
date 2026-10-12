package main

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

// startRun starts run in the background and returns a channel carrying
// its result, plus a function that cancels it. The test reads the channel
// for its normal-path assertions. startRun also registers a cleanup that
// cancels run, calls releases (the release functions of any startup hooks
// the test installed), and then waits for run to return. Cleanups run in
// reverse registration order, and hooks are installed before run starts,
// so without releases here this cleanup would wait on a run still blocked
// in a hook. Waiting matters because run keeps its listeners, on fixed
// ports, open through its shutdown drain: a test that failed early and
// returned at once would leave them bound for the next test.
func startRun(t *testing.T, args []string, stderr io.Writer, releases ...func()) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		errCh <- run(ctx, args, stderr)
	}()
	t.Cleanup(func() {
		cancel()
		for _, release := range releases {
			release()
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("run was still running 30s after the test ended; later tests binding its ports may fail")
		}
	})
	return errCh, cancel
}

// releaser returns a channel and a function that closes it at most once,
// the function also registered as a cleanup: if the test fails before
// calling it, startup blocked on the channel is still released, rather
// than left running with its listeners' ports held, which would break
// every later test that binds them.
func releaser(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

// blockCPUProbe replaces newProbers for the duration of the test with a
// prober set in which every subsystem answers at once except cpu, which
// signals entered and then blocks until release is called. cpu is first
// in canonical order, so it is the first probe of the initial collection.
// others counts calls to every other subsystem's probe.
func blockCPUProbe(t *testing.T) (entered <-chan struct{}, release func(), others *atomic.Int32) {
	t.Helper()
	in := make(chan struct{}, 1)
	out, release := releaser(t)
	others = &atomic.Int32{}

	previous := newProbers
	t.Cleanup(func() { newProbers = previous })
	newProbers = func(*inventory.Store, fs.FS, *slog.Logger) inventory.Probers {
		probers := inventory.Probers{}
		for _, sub := range apiv1.Subsystems() {
			probers[sub] = inventory.ProberFunc(func(context.Context) (any, error) {
				others.Add(1)
				return map[string]any{}, nil
			})
		}
		probers[apiv1.SubsystemCPU] = inventory.ProberFunc(func(context.Context) (any, error) {
			in <- struct{}{}
			<-out
			return map[string]any{"cores": 8}, nil
		})
		return probers
	}
	return in, release, others
}

// holdBeforeAPIServe replaces beforeAPIServe for the duration of the test
// so that startup signals reached and then waits, after the initial
// collection and before the API listener starts, until release is called.
func holdBeforeAPIServe(t *testing.T) (reached <-chan struct{}, release func()) {
	t.Helper()
	in := make(chan struct{}, 1)
	out, release := releaser(t)
	previous := beforeAPIServe
	t.Cleanup(func() { beforeAPIServe = previous })
	beforeAPIServe = func() {
		in <- struct{}{}
		<-out
	}
	return in, release
}

// syncBuffer is an io.Writer safe to read while run's logger writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// apiAccepts reports whether a TCP connection to addr succeeds right now,
// with no retry.
func apiAccepts(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func startupConfig(t *testing.T, apiPort, opsPort string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := `
api:
  bind: { addresses: ["127.0.0.1"], port: ` + apiPort + ` }
  tls: { enabled: false }
  auth: { enabled: false }
  access: { allow: ["127.0.0.1/32"] }
ops:
  bind: { addresses: ["127.0.0.1"], port: ` + opsPort + ` }
log: { level: error }
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func statusCode(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestOpsAnswersDuringTheInitialCollection: while a startup
// probe is still running, the ops listener must already report the process
// live, and not yet ready.
func TestOpsAnswersDuringTheInitialCollection(t *testing.T) {
	entered, release, _ := blockCPUProbe(t)
	path := startupConfig(t, "18445", "19102")

	errCh, cancel := startRun(t, []string{"--config", path}, io.Discard, release)

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the initial collection never reached the cpu probe")
	}
	waitForStatus(t, "http://127.0.0.1:19102/healthz")

	if got := statusCode(t, "http://127.0.0.1:19102/healthz"); got != http.StatusOK {
		t.Errorf("/healthz during the initial collection = %d, want 200", got)
	}
	if got := statusCode(t, "http://127.0.0.1:19102/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz during the initial collection = %d, want 503", got)
	}

	release()

	deadline := time.Now().Add(10 * time.Second)
	for statusCode(t, "http://127.0.0.1:19102/readyz") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("/readyz never became 200 after the initial collection finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Ready must mean the API port accepts now, not eventually.
	if !apiAccepts("127.0.0.1:18445") {
		t.Error("/readyz returned 200 while the API listener was not accepting connections")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

// TestShutdownDuringTheInitialCollection: a stop requested while a startup
// probe is blocked still ends in a clean exit once that probe returns, and
// never opens the API listener.
func TestShutdownDuringTheInitialCollection(t *testing.T) {
	entered, release, _ := blockCPUProbe(t)
	path := startupConfig(t, "18446", "19103")

	errCh, cancel := startRun(t, []string{"--config", path}, io.Discard, release)

	<-entered
	waitForStatus(t, "http://127.0.0.1:19103/healthz")
	cancel()
	// The probers do not watch ctx, so run waits for the probe in flight
	// before returning; release it as a real probe would eventually finish.
	release()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation during the initial collection")
	}
	if resp, err := http.Get("http://127.0.0.1:18446/api/v1/inventory"); err == nil {
		resp.Body.Close()
		t.Error("the API listener opened although shutdown was requested before the initial collection finished")
	}
	if resp, err := http.Get("http://127.0.0.1:19103/healthz"); err == nil {
		resp.Body.Close()
		t.Error("the ops listener is still open after run returned")
	}
}

// TestOpsListenerFailureStopsTheInitialCollection: when the ops listener
// cannot bind, run must stop the initial collection at the next subsystem
// rather than probing every remaining one before returning the error.
func TestOpsListenerFailureStopsTheInitialCollection(t *testing.T) {
	entered, release, others := blockCPUProbe(t)
	path := startupConfig(t, "18447", "19104")

	blocker, err := net.Listen("tcp", "127.0.0.1:19104")
	if err != nil {
		t.Fatalf("occupying the ops port: %v", err)
	}
	defer blocker.Close()

	logs := &syncBuffer{}
	errCh, _ := startRun(t, []string{"--config", path}, logs, release)

	// The occupied port fails the ops bind at once, so run may notice and
	// cancel the collection before RefreshAll has probed anything - then
	// run returns with no probe at all - or after the cpu probe, first in
	// the collection, has started, in which case run must wait for that
	// one probe in flight but start no other.
	var runErr error
	select {
	case <-entered:
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(logs.String(), "listener stopped") {
			if time.Now().After(deadline) {
				t.Fatalf("run never reported the ops bind failure; logs:\n%s", logs)
			}
			time.Sleep(10 * time.Millisecond)
		}
		release()
		select {
		case runErr = <-errCh:
		case <-time.After(15 * time.Second):
			t.Fatal("run did not return after the ops listener failed")
		}
	case runErr = <-errCh:
	case <-time.After(15 * time.Second):
		t.Fatal("run neither started the collection nor returned after the ops listener failed")
	}
	if runErr == nil || !strings.Contains(runErr.Error(), "ops listener") {
		t.Errorf("run returned %v, want the ops listener error", runErr)
	}
	if n := others.Load(); n != 0 {
		t.Errorf("%d more subsystems were probed after the ops listener failed, want 0", n)
	}
}

// TestReadyzWaitsForTheAPIListener: with the initial collection finished
// but the API listener not yet started, /readyz must still be 503, since a
// supervisor would otherwise route clients to a closed port.
func TestReadyzWaitsForTheAPIListener(t *testing.T) {
	_, releaseProbe, _ := blockCPUProbe(t)
	releaseProbe() // No need to hold the collection itself here.
	reached, release := holdBeforeAPIServe(t)
	path := startupConfig(t, "18448", "19105")

	errCh, cancel := startRun(t, []string{"--config", path}, io.Discard, release)

	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("startup never reached the point before the API listener starts")
	}
	// The ops listener binds asynchronously, and with instant probes the
	// collection can finish first, so wait for it to answer.
	waitForStatus(t, "http://127.0.0.1:19105/healthz")
	if got := statusCode(t, "http://127.0.0.1:19105/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz after the collection but before the API listener = %d, want 503", got)
	}

	release()
	deadline := time.Now().Add(10 * time.Second)
	for statusCode(t, "http://127.0.0.1:19105/readyz") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("/readyz never became 200 once the API listener started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !apiAccepts("127.0.0.1:18448") {
		t.Error("/readyz returned 200 while the API listener was not accepting connections")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("run returned %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}
