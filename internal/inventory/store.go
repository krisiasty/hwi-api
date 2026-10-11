package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
)

// Entry is one subsystem's stored state.
type Entry struct {
	Subsystem apiv1.Subsystem
	// Body is the canonical marshalled payload. It is empty only when the
	// subsystem has never collected successfully.
	Body json.RawMessage
	// Digest is a lowercase-hex SHA-256 over Body and Err, recomputed on
	// every Put and Fail, so it changes when the payload changes or when a
	// failure starts or clears, but not when identical data is merely
	// re-collected. The API builds its ETags from it. It is empty only when
	// Body is.
	Digest      string
	CollectedAt time.Time
	// Err is the most recent failure, or empty when healthy. A non-empty
	// Err with a non-empty Body means last-good data is being served.
	Err string
}

// Store holds the marshalled inventory.
//
// Bodies are stored marshalled rather than as structs, so that a stored
// payload keeps nothing alive beyond what it reports, and marshalling once
// per refresh keeps it off the request path.
type Store struct {
	mu        sync.RWMutex
	entries   map[apiv1.Subsystem]Entry
	enabled   map[apiv1.Subsystem]bool
	attempted map[apiv1.Subsystem]bool
}

// NewStore returns a store for the given enabled set. The map must cover
// every subsystem, as EnabledSet guarantees: a subsystem absent from it
// reads as disabled, not enabled.
func NewStore(enabled map[apiv1.Subsystem]bool) *Store {
	e := make(map[apiv1.Subsystem]bool, len(enabled))
	for k, v := range enabled {
		e[k] = v
	}
	return &Store{
		entries:   make(map[apiv1.Subsystem]Entry),
		enabled:   e,
		attempted: make(map[apiv1.Subsystem]bool),
	}
}

// Put records a successful collection, replacing any previous body and
// clearing any previous error.
func (s *Store) Put(sub apiv1.Subsystem, v any, now time.Time) error {
	// encoding/json writes struct fields in declaration order and map keys
	// sorted, and every array is left in the order its prober produced, so
	// unchanged data marshals to the same bytes.
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[sub] = Entry{
		Subsystem:   sub,
		Body:        body,
		Digest:      digest(body, ""),
		CollectedAt: now,
	}
	s.attempted[sub] = true
	return nil
}

// Fail records a failed collection. Any previous body is retained with its
// original collection time, so a consumer can tell "stale but known" from
// "never collected".
func (s *Store) Fail(sub apiv1.Subsystem, cause error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.attempted[sub] = true
	msg := "collection failed"
	if cause != nil {
		msg = cause.Error()
	}
	existing := s.entries[sub]
	existing.Subsystem = sub
	existing.Err = msg
	if len(existing.Body) > 0 {
		existing.Digest = digest(existing.Body, msg)
	}
	s.entries[sub] = existing
}

// digest hashes body and errMsg together. A NUL separates them:
// encoding/json never writes a raw NUL byte, so no body/errMsg pair can collide
// with a different split of the same bytes.
func digest(body []byte, errMsg string) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte{0})
	h.Write([]byte(errMsg))
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns one subsystem's entry. A disabled subsystem is never
// returned, because it is intentionally absent rather than missing.
//
// The returned Entry's Body is a copy, so the caller cannot mutate the
// store's stored bytes through it.
func (s *Store) Get(sub apiv1.Subsystem) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled[sub] {
		return Entry{}, false
	}
	e, ok := s.entries[sub]
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(e), true
}

// Snapshot returns every enabled subsystem that has an entry, in the
// canonical order so output never depends on map iteration.
//
// Each returned Entry's Body is a copy, so the caller cannot mutate the
// store's stored bytes through it.
func (s *Store) Snapshot() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Entry, 0, len(s.entries))
	for _, sub := range apiv1.Subsystems() {
		if !s.enabled[sub] {
			continue
		}
		if e, ok := s.entries[sub]; ok {
			out = append(out, cloneEntry(e))
		}
	}
	return out
}

// Disabled lists the subsystems switched off in configuration.
func (s *Store) Disabled() []apiv1.Subsystem {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []apiv1.Subsystem
	for _, sub := range apiv1.Subsystems() {
		if !s.enabled[sub] {
			out = append(out, sub)
		}
	}
	return out
}

// Ready reports whether every enabled subsystem has been attempted at
// least once, which is what completing the first refresh cycle means. A
// failed attempt counts: readiness is about the cycle having run, not about
// every probe succeeding.
func (s *Store) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for sub, on := range s.enabled {
		if on && !s.attempted[sub] {
			return false
		}
	}
	return true
}

// cloneEntry copies e, including its Body, so a caller holding the returned
// Entry cannot mutate the store's stored bytes by writing through the
// slice header it would otherwise share with the stored copy.
func cloneEntry(e Entry) Entry {
	if e.Body != nil {
		b := make(json.RawMessage, len(e.Body))
		copy(b, e.Body)
		e.Body = b
	}
	return e
}
