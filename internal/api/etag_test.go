package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/inventory"
)

// etagCase drives one route through a store mutation and reports whether a
// client revalidating with the ETag it saw before the mutation gets 304.
type etagCase struct {
	name   string
	mutate func(*inventory.Store)
	want   int
}

// etagCases are the transitions a validator must handle. A weak ETag covers the
// payload and the error state but deliberately not collected_at: identical
// hardware re-collected on the next refresh must still revalidate as 304,
// or a sync across many hosts transfers every host's payload after every refresh.
var etagCases = []etagCase{
	{
		name: "identical payload at a later collection time",
		mutate: func(s *inventory.Store) {
			_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected.Add(time.Hour))
		},
		want: http.StatusNotModified,
	},
	{
		name: "changed payload",
		mutate: func(s *inventory.Store) {
			_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 16}, collected.Add(time.Hour))
		},
		want: http.StatusOK,
	},
	{
		name: "retained payload after a failed refresh",
		mutate: func(s *inventory.Store) {
			s.Fail(apiv1.SubsystemCPU, errors.New("sysfs unreadable"), collected.Add(time.Hour))
		},
		want: http.StatusOK,
	},
}

func TestETagTracksPayloadAndErrors(t *testing.T) {
	for _, route := range []string{"/api/v1/inventory/cpu", "/api/v1/inventory"} {
		for _, tc := range etagCases {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				var store *inventory.Store
				h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
					store = s
					_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
				})

				etag := get(t, h, route, nil).Header().Get("ETag")
				tc.mutate(store)

				w := get(t, h, route, map[string]string{"If-None-Match": etag})
				if w.Code != tc.want {
					t.Errorf("status = %d, want %d", w.Code, tc.want)
				}
			})
		}
	}
}

// TestETagChangesWhenAFailureClears covers the reverse transition: a
// client that saw the error must not be told nothing changed once a
// refresh succeeds again with the same payload.
func TestETagChangesWhenAFailureClears(t *testing.T) {
	for _, route := range []string{"/api/v1/inventory/cpu", "/api/v1/inventory"} {
		t.Run(route, func(t *testing.T) {
			var store *inventory.Store
			h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
				store = s
				_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
				s.Fail(apiv1.SubsystemCPU, errors.New("sysfs unreadable"), collected.Add(time.Minute))
			})

			etag := get(t, h, route, nil).Header().Get("ETag")
			_ = store.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected.Add(time.Hour))

			if w := get(t, h, route, map[string]string{"If-None-Match": etag}); w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 once the error has cleared", w.Code)
			}
		})
	}
}

func TestETagIsWeak(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
	})
	for _, route := range []string{"/api/v1/inventory/cpu", "/api/v1/inventory"} {
		etag := get(t, h, route, nil).Header().Get("ETag")
		if !strings.HasPrefix(etag, `W/"`) || !strings.HasSuffix(etag, `"`) {
			t.Errorf("%s: ETag = %s, want a weak validator W/\"...\"", route, etag)
		}
	}
}

// TestIfNoneMatchUsesWeakComparison: RFC 9110 section 13.1.2 makes
// If-None-Match a weak comparison, so the opaque tag matches with or
// without the W/ prefix.
func TestIfNoneMatchUsesWeakComparison(t *testing.T) {
	h := newTestAPI(t, map[apiv1.Subsystem]bool{apiv1.SubsystemCPU: true}, func(s *inventory.Store) {
		_ = s.Put(apiv1.SubsystemCPU, map[string]any{"cores": 8}, collected)
	})
	etag := get(t, h, "/api/v1/inventory/cpu", nil).Header().Get("ETag")
	strong := strings.TrimPrefix(etag, "W/")

	if w := get(t, h, "/api/v1/inventory/cpu", map[string]string{"If-None-Match": strong}); w.Code != http.StatusNotModified {
		t.Errorf("If-None-Match %s against %s: status = %d, want 304", strong, etag, w.Code)
	}
}
