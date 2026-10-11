package inventory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestBuildSchedulesAppliesDefaultsAndOverrides(t *testing.T) {
	cfg := config.RefreshConfig{
		Interval: config.Duration(time.Minute),
		Subsystems: map[apiv1.Subsystem]config.SubsystemConfig{
			apiv1.SubsystemPCI:     {Interval: config.Duration(30 * time.Minute)},
			apiv1.SubsystemUdevDRM: {Enabled: boolPtr(false)},
		},
	}

	schedules := BuildSchedules(cfg)

	byName := map[apiv1.Subsystem]Schedule{}
	for _, s := range schedules {
		byName[s.Subsystem] = s
	}
	if _, ok := byName[apiv1.SubsystemUdevDRM]; ok {
		t.Error("a disabled subsystem must not be scheduled")
	}
	if got := byName[apiv1.SubsystemPCI].Interval; got != 30*time.Minute {
		t.Errorf("pci interval = %s, want the override", got)
	}
	if got := byName[apiv1.SubsystemCPU].Interval; got != config.DefaultIntervals[apiv1.SubsystemCPU] {
		t.Errorf("cpu interval = %s, want the default", got)
	}
}

func TestBuildSchedulesRaisesIntervalsToTheLoopFloor(t *testing.T) {
	cfg := config.RefreshConfig{
		Interval: config.Duration(10 * time.Minute),
		Subsystems: map[apiv1.Subsystem]config.SubsystemConfig{
			apiv1.SubsystemNetwork: {Interval: config.Duration(time.Second)},
		},
	}

	for _, s := range BuildSchedules(cfg) {
		if s.Interval < 10*time.Minute {
			t.Errorf("%s interval %s is below the loop tick", s.Subsystem, s.Interval)
		}
	}
}

func TestBuildSchedulesOffsetsAreDistinctAndDeterministic(t *testing.T) {
	cfg := config.RefreshConfig{Interval: config.Duration(time.Minute)}

	first := BuildSchedules(cfg)
	second := BuildSchedules(cfg)

	seen := map[time.Duration]bool{}
	for i, s := range first {
		if s.Offset != second[i].Offset {
			t.Fatalf("offsets are not deterministic: %s then %s", s.Offset, second[i].Offset)
		}
		if seen[s.Offset] {
			t.Errorf("offset %s is assigned to more than one subsystem", s.Offset)
		}
		seen[s.Offset] = true
	}
	if first[0].Offset != 0 {
		t.Errorf("first offset = %s, want 0 so a restart produces data at once", first[0].Offset)
	}
}

func TestRefresherRefreshAllPopulatesTheStore(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}
	store := NewStore(enabled)
	probers := Probers{apiv1.SubsystemCPU: ProberFunc(func(context.Context) (any, error) {
		return map[string]any{"cores": 8}, nil
	})}
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Minute}}

	r := NewRefresher(store, probers, schedules, quietLogger(), nil)
	r.RefreshAll(context.Background(), t0)

	if !store.Ready() {
		t.Error("RefreshAll should complete the first cycle")
	}
	got, _ := store.Get(apiv1.SubsystemCPU)
	if string(got.Body) != `{"cores":8}` {
		t.Errorf("Body = %s", got.Body)
	}
}

func TestRefresherRecordsFailuresWithoutLosingData(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}
	store := NewStore(enabled)

	fail := false
	probers := Probers{apiv1.SubsystemCPU: ProberFunc(func(context.Context) (any, error) {
		if fail {
			return nil, errors.New("probe exploded")
		}
		return map[string]any{"cores": 8}, nil
	})}
	r := NewRefresher(store, probers, []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Minute}}, quietLogger(), nil)

	r.RefreshAll(context.Background(), t0)
	fail = true
	r.RefreshAll(context.Background(), t0.Add(time.Minute))

	got, _ := store.Get(apiv1.SubsystemCPU)
	if string(got.Body) != `{"cores":8}` {
		t.Errorf("Body = %s, want the last good payload retained", got.Body)
	}
	if got.Err == "" {
		t.Error("the failure should be recorded")
	}
}

func TestRefresherDueRespectsIntervalsAndOffsets(t *testing.T) {
	schedules := []Schedule{
		{Subsystem: apiv1.SubsystemCPU, Interval: 5 * time.Minute, Offset: 0},
		{Subsystem: apiv1.SubsystemPCI, Interval: 30 * time.Minute, Offset: 3 * time.Second},
	}
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemPCI: true})
	r := NewRefresher(store, Probers{}, schedules, quietLogger(), nil)
	r.Start(t0)

	if got := r.Due(t0.Add(time.Minute)); len(got) != 0 {
		t.Errorf("Due too early = %v, want none", got)
	}
	due := r.Due(t0.Add(6 * time.Minute))
	if len(due) != 1 || due[0] != apiv1.SubsystemCPU {
		t.Errorf("Due = %v, want [cpu]", due)
	}
	// Marking it refreshed pushes it out by a full interval.
	r.MarkRefreshed(apiv1.SubsystemCPU, t0.Add(6*time.Minute))
	if got := r.Due(t0.Add(7 * time.Minute)); len(got) != 0 {
		t.Errorf("Due right after a refresh = %v, want none", got)
	}
	if got := r.Due(t0.Add(31 * time.Minute)); len(got) != 2 {
		t.Errorf("Due = %v, want both subsystems", got)
	}
}

func TestRefresherRunStopsOnContextCancellation(t *testing.T) {
	store := NewStore(map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true})
	probers := Probers{apiv1.SubsystemCPU: ProberFunc(func(context.Context) (any, error) { return map[string]any{}, nil })}
	r := NewRefresher(store, probers, []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Millisecond}}, quietLogger(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// TestRefresherConcurrentRefreshOneAndDue exercises RefreshOne against Due
// and MarkRefreshed for the same subsystem, so `go test -race` has real
// interleaving to catch a missing lock on nextDue, trackers, or the
// RepeatTracker each subsystem owns. This mirrors the loop calling RefreshOne
// on a tick while a forced-refresh request calls RefreshOne directly.
func TestRefresherConcurrentRefreshOneAndDue(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}
	store := NewStore(enabled)

	n := 0
	var mu sync.Mutex
	probers := Probers{apiv1.SubsystemCPU: ProberFunc(func(context.Context) (any, error) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if cur%3 == 0 {
			return nil, errors.New("probe exploded")
		}
		return map[string]any{"n": cur}, nil
	})}
	schedules := []Schedule{{Subsystem: apiv1.SubsystemCPU, Interval: time.Millisecond}}
	r := NewRefresher(store, probers, schedules, quietLogger(), nil)
	r.Start(t0)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The "loop" side: repeatedly ask what's due and refresh it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			now := t0.Add(time.Duration(i) * time.Millisecond)
			for _, sub := range r.Due(now) {
				r.RefreshOne(context.Background(), sub, now)
			}
			i++
		}
	}()

	// The "forced refresh" side: calls RefreshOne directly for the same
	// subsystem, concurrently with the loop above.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			now := t0.Add(time.Duration(i) * time.Millisecond)
			r.RefreshOne(context.Background(), apiv1.SubsystemCPU, now)
			i++
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func boolPtr(b bool) *bool { return &b }
