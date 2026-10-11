package inventory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

// slowProber advances clock by d while "probing", as a probe that takes d
// would, and then succeeds or fails.
func slowProber(clock *fakeClock, d time.Duration, err error) Prober {
	return ProberFunc(func(context.Context) (any, error) {
		clock.Advance(d)
		if err != nil {
			return nil, err
		}
		return map[string]any{"took": d.String()}, nil
	})
}

// TestRefreshAllStampsEachSubsystemWithItsOwnTime: the
// startup cycle probes sequentially under one shared now, and each entry
// must record when its own probe finished, not that shared now, which
// would date a later subsystem before its probe even started.
func TestRefreshAllStampsEachSubsystemWithItsOwnTime(t *testing.T) {
	clock := newFakeClock(t0)
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemMemory: true})
	probers := Probers{
		apiv1.SubsystemCPU:    slowProber(clock, 30*time.Second, nil),
		apiv1.SubsystemMemory: slowProber(clock, 10*time.Second, nil),
	}
	schedules := []Schedule{
		{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour},
		{Subsystem: apiv1.SubsystemMemory, Interval: time.Hour},
	}
	r := NewRefresher(store, probers, schedules, quietLogger(), nil)
	r.clock = clock.Now

	r.RefreshAll(context.Background(), t0)

	for sub, want := range map[apiv1.Subsystem]time.Time{
		apiv1.SubsystemCPU:    t0.Add(30 * time.Second),
		apiv1.SubsystemMemory: t0.Add(40 * time.Second),
	} {
		got, _ := store.Get(sub)
		if !got.CollectedAt.Equal(want) {
			t.Errorf("%s CollectedAt = %s, want when its own probe finished, %s", sub, got.CollectedAt, want)
		}
	}
}

// The next refresh is scheduled a full interval after the probe actually
// finished, not after the moment the batch it ran in was requested.
func TestNextRefreshIsDueAnIntervalAfterCompletion(t *testing.T) {
	clock := newFakeClock(t0)
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour}}
	r := NewRefresher(store, Probers{apiv1.SubsystemCPU: slowProber(clock, 45*time.Second, nil)}, schedules, quietLogger(), nil)
	r.clock = clock.Now

	r.RefreshOne(context.Background(), apiv1.SubsystemCPU, t0)

	finished := t0.Add(45 * time.Second)
	if due := r.Due(finished.Add(time.Hour - time.Nanosecond)); len(due) != 0 {
		t.Errorf("Due just before an interval after completion = %v, want nothing", due)
	}
	if due := r.Due(finished.Add(time.Hour)); len(due) != 1 {
		t.Errorf("Due an interval after completion = %v, want cpu", due)
	}
}

// A failed refresh keeps the last good collection time, however long the
// failing probe took, so stale data never looks fresh.
func TestFailedRefreshKeepsTheLastGoodCollectionTime(t *testing.T) {
	clock := newFakeClock(t0)
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour}}

	good := NewRefresher(store, Probers{apiv1.SubsystemCPU: slowProber(clock, 5*time.Second, nil)}, schedules, quietLogger(), nil)
	good.clock = clock.Now
	good.RefreshOne(context.Background(), apiv1.SubsystemCPU, t0)
	lastGood := t0.Add(5 * time.Second)

	clock.Advance(time.Hour)
	failing := NewRefresher(store, Probers{apiv1.SubsystemCPU: slowProber(clock, 20*time.Second, errors.New("probe failed"))}, schedules, quietLogger(), nil)
	failing.clock = clock.Now
	failing.RefreshOne(context.Background(), apiv1.SubsystemCPU, clock.Now())

	got, _ := store.Get(apiv1.SubsystemCPU)
	if !got.CollectedAt.Equal(lastGood) {
		t.Errorf("CollectedAt after a failure = %s, want the last good %s", got.CollectedAt, lastGood)
	}
	if got.Err != "probe failed" {
		t.Errorf("Err = %q, want the failure recorded", got.Err)
	}
}

// TestStartKeepsEachStartupProbesCompletionTime covers the production
// sequence, RefreshAll then Start: the offsets stagger the next refreshes
// from each subsystem's own completion time, not from the end of the
// whole startup cycle, which would hold back a subsystem that finished
// early.
func TestStartKeepsEachStartupProbesCompletionTime(t *testing.T) {
	clock := newFakeClock(t0)
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemMemory: true})
	probers := Probers{
		apiv1.SubsystemCPU:    slowProber(clock, 30*time.Second, nil),
		apiv1.SubsystemMemory: slowProber(clock, 10*time.Second, nil),
	}
	schedules := []Schedule{
		{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour, Offset: 0},
		{Subsystem: apiv1.SubsystemMemory, Interval: time.Hour, Offset: 2 * time.Second},
	}
	r := NewRefresher(store, probers, schedules, quietLogger(), nil)
	r.clock = clock.Now

	r.RefreshAll(context.Background(), t0)
	r.Start(clock.Now()) // the end of the startup cycle, t0+40s

	for sub, want := range map[apiv1.Subsystem]time.Time{
		apiv1.SubsystemCPU:    t0.Add(30 * time.Second).Add(time.Hour),                      // finished at +30s, offset 0
		apiv1.SubsystemMemory: t0.Add(40 * time.Second).Add(time.Hour).Add(2 * time.Second), // finished at +40s, offset 2s
	} {
		for _, d := range r.Due(want.Add(-time.Nanosecond)) {
			if d == sub {
				t.Errorf("%s is due before %s, its completion time plus interval and offset", sub, want)
			}
		}
		due := false
		for _, d := range r.Due(want) {
			due = due || d == sub
		}
		if !due {
			t.Errorf("%s is not due at %s, its completion time plus interval and offset", sub, want)
		}
	}
}

// A subsystem no startup refresh reached is due an interval plus its
// offset after Start, as before.
func TestStartSchedulesAnUnrefreshedSubsystemFromNow(t *testing.T) {
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Hour, Offset: 4 * time.Second}}
	r := NewRefresher(store, Probers{}, schedules, quietLogger(), nil)

	r.Start(t0)

	want := t0.Add(time.Hour).Add(4 * time.Second)
	if due := r.Due(want.Add(-time.Nanosecond)); len(due) != 0 {
		t.Errorf("Due before %s = %v, want nothing", want, due)
	}
	if due := r.Due(want); len(due) != 1 {
		t.Errorf("Due at %s = %v, want cpu", want, due)
	}
}
