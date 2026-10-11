package inventory

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

var t0 = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

func enabledAll() map[apiv1.Subsystem]bool {
	m := map[apiv1.Subsystem]bool{}
	for _, s := range apiv1.Subsystems() {
		m[s] = true
	}
	return m
}

func TestStorePutThenGet(t *testing.T) {
	s := NewStore(enabledAll())
	if err := s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, t0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := s.Get(apiv1.SubsystemCPU)
	if !ok {
		t.Fatal("Get returned nothing after Put")
	}
	if string(got.Body) != `{"cores":8}` {
		t.Errorf("Body = %s", got.Body)
	}
	if !got.CollectedAt.Equal(t0) {
		t.Errorf("CollectedAt = %s", got.CollectedAt)
	}
	if got.Err != "" {
		t.Errorf("Err = %q, want empty", got.Err)
	}
	if len(got.Digest) != 64 {
		t.Errorf("Digest = %q, want 64 hex characters", got.Digest)
	}
}

func TestStoreDigestTracksContentAndErrors(t *testing.T) {
	s := NewStore(enabledAll())
	_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, t0)
	first, _ := s.Get(apiv1.SubsystemCPU)

	// Same content, later time: the digest must not move.
	_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, t0.Add(time.Hour))
	same, _ := s.Get(apiv1.SubsystemCPU)
	if same.Digest != first.Digest {
		t.Errorf("Digest changed for identical content: %s then %s", first.Digest, same.Digest)
	}

	// A failure that retains the payload: the digest must move.
	s.Fail(apiv1.SubsystemCPU, errors.New("sysfs unreadable"), t0.Add(2*time.Hour))
	failed, _ := s.Get(apiv1.SubsystemCPU)
	if failed.Digest == first.Digest {
		t.Error("Digest did not change when a failure was recorded")
	}

	// Recovery with the same content: back to the healthy digest.
	_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, t0.Add(3*time.Hour))
	recovered, _ := s.Get(apiv1.SubsystemCPU)
	if recovered.Digest != first.Digest {
		t.Errorf("Digest after recovery = %s, want the healthy %s", recovered.Digest, first.Digest)
	}

	// Changed content: the digest must move.
	_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 16}, t0.Add(4*time.Hour))
	changed, _ := s.Get(apiv1.SubsystemCPU)
	if changed.Digest == first.Digest {
		t.Error("Digest did not change when content changed")
	}
}

func TestStoreFailRetainsLastGoodData(t *testing.T) {
	s := NewStore(enabledAll())
	_ = s.Put(apiv1.SubsystemPCI, map[string]any{"devices": 3}, t0)

	s.Fail(apiv1.SubsystemPCI, errors.New("sysfs unreadable"), t0.Add(time.Hour))

	got, ok := s.Get(apiv1.SubsystemPCI)
	if !ok {
		t.Fatal("a failing refresh must not remove the entry")
	}
	if string(got.Body) != `{"devices":3}` {
		t.Errorf("Body = %s, want the last good payload", got.Body)
	}
	if !got.CollectedAt.Equal(t0) {
		t.Errorf("CollectedAt = %s, want the time of the last success", got.CollectedAt)
	}
	if got.Err == "" {
		t.Error("Err must record that the refresh is failing")
	}
}

func TestStoreFailWithNoPriorDataHasNoBody(t *testing.T) {
	s := NewStore(enabledAll())
	s.Fail(apiv1.SubsystemPCI, errors.New("sysfs unreadable"), t0)

	got, ok := s.Get(apiv1.SubsystemPCI)
	if !ok {
		t.Fatal("Get should report the failure")
	}
	if len(got.Body) != 0 {
		t.Errorf("Body = %s, want nothing", got.Body)
	}
}

func TestStorePutClearsAPreviousError(t *testing.T) {
	s := NewStore(enabledAll())
	s.Fail(apiv1.SubsystemCPU, errors.New("transient"), t0)
	_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 4}, t0.Add(time.Minute))

	got, _ := s.Get(apiv1.SubsystemCPU)
	if got.Err != "" {
		t.Errorf("Err = %q, want it cleared by a successful refresh", got.Err)
	}
}

func TestStoreSnapshotUsesCanonicalOrderAndSkipsDisabled(t *testing.T) {
	enabled := enabledAll()
	enabled[apiv1.SubsystemUdevDRM] = false
	s := NewStore(enabled)

	_ = s.Put(apiv1.SubsystemNetwork, map[string]any{"n": 1}, t0)
	_ = s.Put(apiv1.SubsystemCPU, map[string]any{"n": 2}, t0)

	got := s.Snapshot()
	if len(got) != 2 {
		t.Fatalf("Snapshot has %d entries, want 2", len(got))
	}
	if got[0].Subsystem != apiv1.SubsystemCPU {
		t.Errorf("Snapshot()[0] = %s, want cpu first per canonical order", got[0].Subsystem)
	}

	disabled := s.Disabled()
	if len(disabled) != 1 || disabled[0] != apiv1.SubsystemUdevDRM {
		t.Errorf("Disabled() = %v, want [udev-drm]", disabled)
	}
	if _, ok := s.Get(apiv1.SubsystemUdevDRM); ok {
		t.Error("a disabled subsystem must not be retrievable")
	}
}

func TestStoreReadyWaitsForEveryEnabledSubsystem(t *testing.T) {
	enabled := map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true, apiv1.SubsystemOS: true}
	s := NewStore(enabled)

	if s.Ready() {
		t.Error("a fresh store must not be ready")
	}
	_ = s.Put(apiv1.SubsystemCPU, map[string]any{}, t0)
	if s.Ready() {
		t.Error("one subsystem is not a full cycle")
	}
	// An attempt counts, even a failed one: readiness means the first cycle
	// finished, not that everything succeeded.
	s.Fail(apiv1.SubsystemOS, errors.New("nope"), t0)
	if !s.Ready() {
		t.Error("store should be ready once every enabled subsystem has been attempted")
	}
}

// TestStoreGetBodyIsIsolatedFromStore guards the sharing rule described in
// Get's doc comment: a caller must not be able to corrupt the store's
// stored bytes by writing into the slice it was handed back.
func TestStoreGetBodyIsIsolatedFromStore(t *testing.T) {
	s := NewStore(enabledAll())
	_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, t0)

	got, _ := s.Get(apiv1.SubsystemCPU)
	if len(got.Body) == 0 {
		t.Fatal("expected a non-empty body to mutate")
	}
	got.Body[0] = 'X'

	again, _ := s.Get(apiv1.SubsystemCPU)
	if string(again.Body) != `{"cores":8}` {
		t.Errorf("stored body was mutated through a returned Entry: %s", again.Body)
	}
}

// TestStoreConcurrentAccess exercises concurrent Put/Fail against concurrent
// Get/Snapshot so `go test -race` has real interleaving to catch a missing
// or misplaced lock.
func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore(enabledAll())
	subs := apiv1.Subsystems()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i, sub := range subs {
		wg.Add(1)
		go func(i int, sub apiv1.Subsystem) {
			defer wg.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				now := t0.Add(time.Duration(n) * time.Millisecond)
				if n%5 == 0 {
					s.Fail(sub, errors.New("transient"), now)
				} else {
					_ = s.Put(sub, map[string]any{"n": n}, now)
				}
				n++
			}
		}(i, sub)
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if i%2 == 0 {
					_, _ = s.Get(subs[i%len(subs)])
				} else {
					_ = s.Snapshot()
				}
				_ = s.Ready()
				_ = s.Disabled()
			}
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// Put keeps every array in the order the prober produced it. Some orders
// carry meaning: a topology node's distances[i] is its distance to node i,
// so sorting node 1's [21, 10] would claim it is local to node 0.
func TestStorePutKeepsArrayOrder(t *testing.T) {
	s := NewStore(enabledAll())
	payload := map[string]any{
		"nodes": []map[string]any{
			{"id": 0, "distances": []int{10, 21}},
			{"id": 1, "distances": []int{21, 10}},
		},
		"page_sizes": []uint64{2097152, 1073741824},
		"evidence":   []string{"sys.hypervisor.type", "dmi.sys_vendor"},
	}
	if err := s.Put(apiv1.SubsystemTopology, payload, t0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, _ := s.Get(apiv1.SubsystemTopology)
	want := `{"evidence":["sys.hypervisor.type","dmi.sys_vendor"],` +
		`"nodes":[{"distances":[10,21],"id":0},{"distances":[21,10],"id":1}],` +
		`"page_sizes":[2097152,1073741824]}`
	if string(got.Body) != want {
		t.Errorf("Body = %s\nwant   %s", got.Body, want)
	}
}

// The same content always stores the same bytes and digest: map keys are
// sorted, however the map happens to iterate.
func TestStorePutIsStableForTheSameContent(t *testing.T) {
	payload := func() map[string]any {
		m := map[string]any{}
		for _, k := range []string{"q", "w", "e", "r", "t", "y", "u", "i", "o", "p"} {
			m[k] = map[string]int{k + "1": 1, k + "2": 2, k + "3": 3}
		}
		return m
	}
	s := NewStore(enabledAll())
	if err := s.Put(apiv1.SubsystemCPU, payload(), t0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	first, _ := s.Get(apiv1.SubsystemCPU)
	for range 50 {
		if err := s.Put(apiv1.SubsystemCPU, payload(), t0); err != nil {
			t.Fatalf("Put: %v", err)
		}
		again, _ := s.Get(apiv1.SubsystemCPU)
		if string(again.Body) != string(first.Body) || again.Digest != first.Digest {
			t.Fatalf("the same content stored differently:\n%s\n%s", first.Body, again.Body)
		}
	}
}

func TestStorePutKeepsLargeIntegersExact(t *testing.T) {
	s := NewStore(enabledAll())
	payload := map[string]any{
		"max_int64":  int64(9223372036854775807),
		"max_uint64": uint64(18446744073709551615),
		"above_2_53": int64(9007199254740993), // not representable as a float64
	}
	if err := s.Put(apiv1.SubsystemMemory, payload, t0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, _ := s.Get(apiv1.SubsystemMemory)
	want := `{"above_2_53":9007199254740993,"max_int64":9223372036854775807,"max_uint64":18446744073709551615}`
	if string(got.Body) != want {
		t.Errorf("Body = %s, want %s", got.Body, want)
	}
}
