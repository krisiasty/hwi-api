package inventory

import (
	"context"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

// gateProber lets a test decide exactly when each probe call returns and
// what it returns. Every call announces itself on entered, then blocks
// until the test sends its result on release.
type gateProber struct {
	entered chan struct{}
	release chan any
}

func newGateProber() *gateProber {
	return &gateProber{entered: make(chan struct{}), release: make(chan any)}
}

func (g *gateProber) Probe(context.Context) (any, error) {
	g.entered <- struct{}{}
	return <-g.release, nil
}

func newGatedRefresher(g *gateProber) (*Refresher, *Store) {
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour}}
	return NewRefresher(store, Probers{apiv1.SubsystemCPU: g}, schedules, quietLogger(), nil), store
}

// TestRefreshOneAppliesResultsInStartOrder covers this scenario: a
// scheduled refresh A starts and stalls, a forced refresh B starts later.
// B must not probe until A has finished, so A's older observation can
// never land after, and overwrite, B's newer one.
func TestRefreshOneAppliesResultsInStartOrder(t *testing.T) {
	g := newGateProber()
	r, store := newGatedRefresher(g)
	later := t0.Add(time.Minute)
	clock := newFakeClock(t0)
	r.clock = clock.Now

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		r.RefreshOne(context.Background(), apiv1.SubsystemCPU, t0)
	}()
	<-g.entered // A is inside its probe.

	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		r.RefreshOne(context.Background(), apiv1.SubsystemCPU, later)
	}()

	select {
	case <-g.entered:
		t.Fatal("B started probing while A's probe for the same subsystem was still running")
	case <-time.After(100 * time.Millisecond):
	}

	clock.Advance(10 * time.Second)
	g.release <- map[string]any{"cores": 8} // A's older observation.
	<-aDone
	<-g.entered // Only now does B probe.
	clock.Advance(10 * time.Second)
	bFinished := clock.Now()
	g.release <- map[string]any{"cores": 16} // B's newer observation.
	<-bDone

	got, _ := store.Get(apiv1.SubsystemCPU)
	if string(got.Body) != `{"cores":16}` {
		t.Errorf("Body = %s, want B's newer observation", got.Body)
	}
	if !got.CollectedAt.Equal(bFinished) {
		t.Errorf("CollectedAt = %s, want when B's probe finished, %s", got.CollectedAt, bFinished)
	}
	if due := r.Due(bFinished.Add(time.Hour - time.Nanosecond)); len(due) != 0 {
		t.Errorf("Due just before B's next due time = %v, want nothing: the schedule must follow B", due)
	}
}

// TestRefreshOneSkipsAnAttemptOlderThanTheLastRun covers waiters released
// out of order: an attempt requested before the latest one that ran would
// only fetch data no newer than what is stored, and applying it would move
// collected_at and the schedule backward.
func TestRefreshOneSkipsAnAttemptOlderThanTheLastRun(t *testing.T) {
	calls := 0
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	probers := Probers{apiv1.SubsystemCPU: ProberFunc(func(context.Context) (any, error) {
		calls++
		return map[string]any{"call": calls}, nil
	})}
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour}}
	r := NewRefresher(store, probers, schedules, quietLogger(), nil)
	collected := t0.Add(3 * time.Minute)
	r.clock = newFakeClock(collected).Now

	newer := t0.Add(2 * time.Minute)
	r.RefreshOne(context.Background(), apiv1.SubsystemCPU, newer)
	r.RefreshOne(context.Background(), apiv1.SubsystemCPU, t0.Add(time.Minute))

	if calls != 1 {
		t.Errorf("probe calls = %d, want 1: the older attempt must not probe", calls)
	}
	got, _ := store.Get(apiv1.SubsystemCPU)
	if !got.CollectedAt.Equal(collected) {
		t.Errorf("CollectedAt = %s, want the newer attempt's collection time %s", got.CollectedAt, collected)
	}
	if due := r.Due(collected.Add(time.Hour - time.Nanosecond)); len(due) != 0 {
		t.Errorf("Due = %v, want nothing: the older attempt must not pull the schedule back", due)
	}
}

// TestRefreshOneStopsWaitingWhenItsContextEnds: a forced refresh queued
// behind a stalled probe must return once its request is gone, rather
// than hold a request slot until the probe finishes.
func TestRefreshOneStopsWaitingWhenItsContextEnds(t *testing.T) {
	g := newGateProber()
	r, _ := newGatedRefresher(g)

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		r.RefreshOne(context.Background(), apiv1.SubsystemCPU, t0)
	}()
	<-g.entered

	ctx, cancel := context.WithCancel(context.Background())
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		r.RefreshOne(ctx, apiv1.SubsystemCPU, t0.Add(time.Minute))
	}()
	cancel()

	select {
	case <-bDone:
	case <-g.entered:
		t.Fatal("B probed concurrently with A instead of waiting")
	case <-time.After(2 * time.Second):
		t.Fatal("B kept waiting after its context was canceled")
	}

	g.release <- map[string]any{"cores": 8}
	<-aDone
}

// TestRefreshOneWithACanceledContextNeverProbes: with the slot free and
// ctx already canceled, both arms of the acquire select are ready and Go
// picks one at random, so without a check after acquiring, roughly half
// of these calls would run a probe for a client that has gone away. The
// loop makes that random choice come up many times.
func TestRefreshOneWithACanceledContextNeverProbes(t *testing.T) {
	calls := 0
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	probers := Probers{apiv1.SubsystemCPU: ProberFunc(func(context.Context) (any, error) {
		calls++
		return map[string]any{}, nil
	})}
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour}}
	r := NewRefresher(store, probers, schedules, quietLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 100 {
		r.RefreshOne(ctx, apiv1.SubsystemCPU, t0.Add(time.Duration(i)*time.Second))
	}
	if calls != 0 {
		t.Errorf("probe calls = %d, want 0 with an already-canceled context", calls)
	}
	if _, ok := store.Get(apiv1.SubsystemCPU); ok {
		t.Error("a canceled attempt must not record anything in the store")
	}
}

// TestRunKeepsOtherSubsystemsMovingWhileAForcedProbeIsBusy: a forced
// refresh holding cpu's slot must not stall the single scheduled loop, so
// memory, also due, is still collected while cpu's probe is blocked, and
// the loop never starts a second cpu probe of its own meanwhile.
func TestRunKeepsOtherSubsystemsMovingWhileAForcedProbeIsBusy(t *testing.T) {
	cpu := newGateProber()
	memoryProbed := make(chan struct{}, 16)
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemMemory: true})
	probers := Probers{
		apiv1.SubsystemCPU: cpu,
		apiv1.SubsystemMemory: ProberFunc(func(context.Context) (any, error) {
			memoryProbed <- struct{}{}
			return map[string]any{}, nil
		}),
	}
	schedules := []Schedule{
		{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour},
		{Subsystem: apiv1.SubsystemMemory, Interval: time.Hour},
	}
	r := NewRefresher(store, probers, schedules, quietLogger(), nil)

	forcedDone := make(chan struct{})
	go func() {
		defer close(forcedDone)
		r.RefreshOne(context.Background(), apiv1.SubsystemCPU, time.Now())
	}()
	<-cpu.entered // The forced cpu probe now holds cpu's slot.

	// Start is not called, so both subsystems are due on the first tick.
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		r.Run(ctx)
	}()

	select {
	case <-memoryProbed:
	case <-cpu.entered:
		t.Fatal("Run started a second cpu probe while the forced one was still running")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not reach memory while a forced cpu probe was blocked")
	}

	cancel()
	<-runDone
	cpu.release <- map[string]any{"cores": 8}
	<-forcedDone
}
