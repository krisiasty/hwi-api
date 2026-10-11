package inventory

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

// probeLog records when each subsystem's probe ran.
type probeLog struct {
	mu    sync.Mutex
	times map[apiv1.Subsystem][]time.Time
}

func (l *probeLog) prober(sub apiv1.Subsystem) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		l.mu.Lock()
		l.times[sub] = append(l.times[sub], time.Now())
		l.mu.Unlock()
		return map[string]any{}, nil
	})
}

func (l *probeLog) snapshot() map[apiv1.Subsystem][]time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[apiv1.Subsystem][]time.Time, len(l.times))
	for k, v := range l.times {
		out[k] = append([]time.Time(nil), v...)
	}
	return out
}

func runFor(t *testing.T, r *Refresher, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(d)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// TestRunHonoursOffsets checks that offsets smaller than any polling
// tick still stagger the probes, not collapse them into one batch.
func TestRunHonoursOffsets(t *testing.T) {
	subs := []apiv1.Subsystem{apiv1.SubsystemCPU, apiv1.SubsystemMemory, apiv1.SubsystemPCI}
	log := &probeLog{times: map[apiv1.Subsystem][]time.Time{}}
	probers := Probers{}
	var schedules []Schedule
	for i, s := range subs {
		probers[s] = log.prober(s)
		schedules = append(schedules, Schedule{Subsystem: s, Interval: time.Hour, Offset: time.Duration(i) * 300 * time.Millisecond})
	}
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemMemory: true, apiv1.SubsystemPCI: true})
	r := NewRefresher(store, probers, schedules, quietLogger(), nil)

	start := time.Now()
	// Start seeds a full interval out; starting it an interval in the past
	// makes each subsystem due exactly at its offset from now.
	r.Start(start.Add(-time.Hour))
	runFor(t, r, 1200*time.Millisecond)

	got := log.snapshot()
	for i, s := range subs {
		if len(got[s]) != 1 {
			t.Fatalf("%s probed %d times, want once", s, len(got[s]))
		}
		at := got[s][0].Sub(start)
		want := time.Duration(i) * 300 * time.Millisecond
		if at < want || at > want+200*time.Millisecond {
			t.Errorf("%s probed at +%s, want at its offset +%s", s, at.Round(time.Millisecond), want)
		}
	}
}

// Intervals are honoured as configured, not rounded up to a tick: with
// no tick, 150ms and 400ms schedules run at clearly different rates. It
// also covers two non-multiple intervals, which must not round up to the
// same tick and run together.
func TestRunHonoursEachInterval(t *testing.T) {
	log := &probeLog{times: map[apiv1.Subsystem][]time.Time{}}
	probers := Probers{apiv1.SubsystemCPU: log.prober(apiv1.SubsystemCPU), apiv1.SubsystemTopology: log.prober(apiv1.SubsystemTopology)}
	schedules := []Schedule{
		{Subsystem: apiv1.SubsystemCPU, Interval: 150 * time.Millisecond},
		{Subsystem: apiv1.SubsystemTopology, Interval: 400 * time.Millisecond},
	}
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemTopology: true})
	r := NewRefresher(store, probers, schedules, quietLogger(), nil)
	r.Start(time.Now().Add(-time.Hour)) // both due at once, then every interval

	runFor(t, r, 1500*time.Millisecond)

	got := log.snapshot()
	cpu, topo := len(got[apiv1.SubsystemCPU]), len(got[apiv1.SubsystemTopology])
	// Nominally 10 and 4 in 1.5s; allow for scheduling slack either way.
	if cpu < 7 || cpu > 11 {
		t.Errorf("cpu probed %d times in 1.5s at a 150ms interval, want about 10", cpu)
	}
	if topo < 3 || topo > 5 {
		t.Errorf("topology probed %d times in 1.5s at a 400ms interval, want about 4", topo)
	}
	for sub, times := range got {
		want := 150 * time.Millisecond
		if sub == apiv1.SubsystemTopology {
			want = 400 * time.Millisecond
		}
		for i := 1; i < len(times); i++ {
			if gap := times[i].Sub(times[i-1]); gap < want-20*time.Millisecond {
				t.Errorf("%s probes %s apart, sooner than its %s interval", sub, gap.Round(time.Millisecond), want)
			}
		}
	}
}

// A subsystem skipped because a forced refresh holds its slot stays due;
// Run must neither spin on it nor drop it, but come back after busyRetry.
func TestRunRetriesABusySubsystemAfterAPause(t *testing.T) {
	gate := newGateProber()
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour}}
	r := NewRefresher(store, Probers{apiv1.SubsystemCPU: gate}, schedules, quietLogger(), nil)

	// A forced refresh, requested before cpu comes due, holds its slot.
	// Its now is in the past so that the scheduled attempt, which comes
	// later, is not skipped as stale once the slot frees.
	forcedDone := make(chan struct{})
	go func() {
		defer close(forcedDone)
		r.RefreshOne(context.Background(), apiv1.SubsystemCPU, time.Now().Add(-time.Hour))
	}()
	<-gate.entered

	// cpu is overdue, so Run's first pass finds it busy and skips it.
	r.Start(time.Now().Add(-2 * time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() { r.Run(ctx); close(runDone) }()

	// Hold the slot a little, then let the forced refresh finish without
	// moving cpu's schedule past now, as a refresh of stale data would.
	time.Sleep(200 * time.Millisecond)
	r.mu.Lock()
	r.nextDue[apiv1.SubsystemCPU] = time.Now().Add(-time.Minute)
	r.mu.Unlock()
	released := time.Now()
	gate.release <- map[string]any{}
	<-forcedDone
	r.mu.Lock()
	r.nextDue[apiv1.SubsystemCPU] = time.Now().Add(-time.Minute)
	r.mu.Unlock()

	select {
	case <-gate.entered:
		// The first pass skipped cpu right after Run started, so the retry
		// is due busyRetry after that: about 800ms after the release 200ms
		// in. A loop spinning on the skipped subsystem would come back
		// almost at once instead.
		waited := time.Since(released)
		if waited < busyRetry/2 {
			t.Errorf("Run retried the busy subsystem %s after the slot freed, want it to pause about %s, not spin", waited.Round(time.Millisecond), busyRetry)
		}
		if waited > busyRetry+500*time.Millisecond {
			t.Errorf("Run retried the busy subsystem after %s, want within about %s", waited.Round(time.Millisecond), busyRetry)
		}
		gate.release <- map[string]any{}
	case <-time.After(busyRetry + 2*time.Second):
		t.Fatal("Run never came back to the subsystem it had skipped as busy")
	}

	cancel()
	<-runDone
}
