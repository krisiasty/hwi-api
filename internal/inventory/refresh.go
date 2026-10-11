package inventory

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/krisiasty/hwi-api/apiv1"
	"github.com/krisiasty/hwi-api/internal/config"
	"github.com/krisiasty/hwi-api/internal/logging"
)

// probeSpacing is the nominal separation between subsystem start offsets,
// enough that every subsystem's probe does not fire together and spike
// host I/O.
const probeSpacing = 2 * time.Second

// Schedule is one subsystem's place in the loop.
type Schedule struct {
	Subsystem apiv1.Subsystem
	Interval  time.Duration
	Offset    time.Duration
}

// EnabledSet reports which subsystems are collected. Absent from
// configuration means enabled: the subsystem set is fixed and closed, so
// "absent means default" is unambiguous.
func EnabledSet(cfg config.RefreshConfig) map[apiv1.Subsystem]bool {
	out := make(map[apiv1.Subsystem]bool, len(apiv1.Subsystems()))
	for _, s := range apiv1.Subsystems() {
		out[s] = true
		if sub, ok := cfg.Subsystems[s]; ok {
			out[s] = sub.IsEnabled()
		}
	}
	return out
}

// BuildSchedules resolves intervals and assigns start offsets, in canonical
// subsystem order so the result is identical on every run.
func BuildSchedules(cfg config.RefreshConfig) []Schedule {
	floor := cfg.Interval.Duration()
	if floor <= 0 {
		floor = time.Minute
	}
	enabled := EnabledSet(cfg)

	var out []Schedule
	for _, s := range apiv1.Subsystems() {
		if !enabled[s] {
			continue
		}
		out = append(out, Schedule{Subsystem: s, Interval: cfg.EffectiveInterval(s)})
	}

	// Offsets are assigned by position rather than drawn randomly: random
	// offsets only separate schedules on average and cluster in practice.
	// The first starts immediately so a restart produces data at once.
	spacing := probeSpacing
	if len(out) > 0 && floor/time.Duration(len(out)) < spacing {
		spacing = floor / time.Duration(len(out))
	}
	for i := range out {
		out[i].Offset = time.Duration(i) * spacing
	}
	return out
}

// Recorder receives probe timings and outcomes. It is an interface, and
// declared here rather than in the metrics package, so that inventory has
// no dependency on metrics.
type Recorder interface {
	ObserveProbe(sub apiv1.Subsystem, d time.Duration, err error)
}

// Refresher keeps the store current.
//
// mu guards nextDue, trackers and lastRun only; it is never held across a
// probe call (Prober.Probe can take tens of milliseconds), so a reader such
// as Due, or a RefreshOne for a different subsystem, is never blocked
// behind a slow probe.
//
// slots holds one single-slot semaphore per subsystem, and RefreshOne holds
// its subsystem's slot from before the probe until the result is applied.
// Two attempts for the same subsystem therefore never overlap. Without
// that, an older attempt that stalled could finish after a newer one and
// overwrite its data, timestamp and schedule with an older observation.
type Refresher struct {
	store     *Store
	probers   Probers
	schedules []Schedule
	log       *slog.Logger
	rec       Recorder
	slots     map[apiv1.Subsystem]chan struct{}

	mu       sync.Mutex
	nextDue  map[apiv1.Subsystem]time.Time
	trackers map[apiv1.Subsystem]*logging.RepeatTracker
	// lastRun is the now of the latest attempt that probed each subsystem.
	lastRun map[apiv1.Subsystem]time.Time

	// clock is time.Now; a test substitutes a fake to control what
	// collection times a probe gets.
	clock func() time.Time
}

// NewRefresher wires a refresher. Call Start before Run or Due. rec may be
// nil, in which case no metrics are recorded.
func NewRefresher(store *Store, probers Probers, schedules []Schedule, log *slog.Logger, rec Recorder) *Refresher {
	slots := make(map[apiv1.Subsystem]chan struct{}, len(probers))
	for sub := range probers {
		slots[sub] = make(chan struct{}, 1)
	}
	return &Refresher{
		store:     store,
		probers:   probers,
		schedules: schedules,
		log:       log,
		rec:       rec,
		slots:     slots,
		nextDue:   make(map[apiv1.Subsystem]time.Time),
		trackers:  make(map[apiv1.Subsystem]*logging.RepeatTracker),
		lastRun:   make(map[apiv1.Subsystem]time.Time),
		clock:     time.Now,
	}
}

// Start staggers the steady-state schedule by adding each subsystem's
// offset to its next due time.
//
// Call Start after the initial full refresh (RefreshAll), not before:
// RefreshAll already delivers "first collection happens immediately" for
// every subsystem, so the offset's job is to stagger the refreshes after
// it. Each refresh RefreshAll ran has already set its subsystem's next due
// time a full interval after that probe finished; Start keeps that and
// adds the offset, so a subsystem that finished early in a slow startup
// cycle is not held back to the end of the cycle, and subsystems that
// finished within moments of each other are still spread apart. A
// subsystem with no next due time yet, which no refresh has reached, is
// due a full interval plus its offset after now. Calling Start first
// would let RefreshAll's refreshes overwrite the offsets it added.
func (r *Refresher) Start(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.schedules {
		due, ok := r.nextDue[s.Subsystem]
		if !ok {
			due = now.Add(s.Interval)
		}
		r.nextDue[s.Subsystem] = due.Add(s.Offset)
	}
}

// Due lists the subsystems whose next collection time has arrived, in
// canonical order.
func (r *Refresher) Due(now time.Time) []apiv1.Subsystem {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []apiv1.Subsystem
	for _, s := range r.schedules {
		due, ok := r.nextDue[s.Subsystem]
		if !ok || !now.Before(due) {
			out = append(out, s.Subsystem)
		}
	}
	return out
}

// MarkRefreshed pushes a subsystem's next due time out by its interval.
func (r *Refresher) MarkRefreshed(sub apiv1.Subsystem, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.schedules {
		if s.Subsystem == sub {
			r.nextDue[sub] = now.Add(s.Interval)
			return
		}
	}
}

// RefreshOne probes a single subsystem and records the outcome. It never
// returns an error: a failed probe is recorded in the store and the loop
// continues.
//
// It may be called concurrently, including for the same subsystem from
// both the scheduled loop and a forced-refresh request. Calls for the same
// subsystem are serialized on its slot (see Refresher), so results are
// applied in the order the probes ran. A call waiting for the slot returns
// without probing if ctx ends first, and so does one whose ctx has ended
// by the time it holds the slot. Once it holds the slot, it also returns
// without probing if now is before the now of the latest attempt that
// already ran: waiters are not released in arrival order, and that later
// attempt probed after this one was requested, so its result is already
// at least as fresh as this call could fetch. Skipping it also keeps
// collected_at and the next due time from moving backward.
func (r *Refresher) RefreshOne(ctx context.Context, sub apiv1.Subsystem, now time.Time) {
	r.refresh(ctx, sub, now, true)
}

// refresh is RefreshOne, with wait choosing what happens when another
// attempt holds sub's slot: wait for it (RefreshOne, RefreshAll), or
// return at once (Run). The scheduled loop must not wait: it is a single
// goroutine, so blocking on one subsystem behind a slow forced refresh
// would also delay every other subsystem due meanwhile. Skipping loses
// nothing, because the attempt holding the slot marks the subsystem
// refreshed when it finishes, and if it is somehow still due, Run comes
// back to it after busyRetry.
func (r *Refresher) refresh(ctx context.Context, sub apiv1.Subsystem, now time.Time, wait bool) {
	prober, ok := r.probers[sub]
	if !ok {
		return
	}

	slot := r.slots[sub]
	if wait {
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			return
		}
	} else {
		select {
		case slot <- struct{}{}:
		default:
			return
		}
	}
	defer func() { <-slot }()

	// With the slot free and ctx already done, both arms of the wait
	// select above are ready and Go may pick the send, so check again:
	// the probers do not watch ctx, and a probe started here would run to
	// completion for a caller that has already gone.
	if ctx.Err() != nil {
		return
	}

	r.mu.Lock()
	if last, ran := r.lastRun[sub]; ran && now.Before(last) {
		r.mu.Unlock()
		return
	}
	r.lastRun[sub] = now
	r.mu.Unlock()

	// now is when this refresh was requested, and only orders attempts
	// (the lastRun check above). What was collected, and when the next
	// refresh is due, are timed from the moment this probe finished: a
	// batch of sequential probes shares one now, so stamping each with it
	// would date a later subsystem before its own probe even started.
	started := r.clock()
	value, err := prober.Probe(ctx)
	done := r.clock()
	if err == nil {
		err = r.store.Put(sub, value, done)
	}
	if r.rec != nil {
		r.rec.ObserveProbe(sub, done.Sub(started), err)
	}

	if err != nil {
		// The store keeps the last good collection time, so a failure
		// never makes stale data look fresh.
		r.store.Fail(sub, err, done)
	}

	r.mu.Lock()
	tracker, ok := r.trackers[sub]
	if !ok {
		tracker = &logging.RepeatTracker{}
		r.trackers[sub] = tracker
	}
	var errLine, recoveredLine string
	if err != nil {
		errLine = tracker.Observe(err.Error(), done)
	} else {
		recoveredLine = tracker.Recovered(done)
	}
	r.mu.Unlock()

	if errLine != "" {
		r.log.Error("subsystem refresh failed", "subsystem", sub, "error", errLine)
	}
	if recoveredLine != "" {
		r.log.Info("subsystem refresh recovered", "subsystem", sub, "detail", recoveredLine)
	}

	r.MarkRefreshed(sub, done)
}

// RefreshAll probes every scheduled subsystem, used for the startup cycle.
func (r *Refresher) RefreshAll(ctx context.Context, now time.Time) {
	for _, s := range r.schedules {
		if ctx.Err() != nil {
			return
		}
		r.RefreshOne(ctx, s.Subsystem, now)
	}
}

// busyRetry is how long Run waits before re-checking a subsystem it had to
// skip because a forced refresh held its slot. It stays due meanwhile, so
// without a pause the loop would spin on it; the forced refresh normally
// finishes and pushes its next due time out well before this elapses.
const busyRetry = time.Second

// Run refreshes subsystems as they come due until ctx is cancelled. It
// sleeps until the earliest next due time across all schedules, rather
// than polling on a fixed tick, so each subsystem runs at its own
// interval and offset exactly, not rounded up to a tick. Probes run one at
// a time, and a subsystem that comes due while an earlier probe is still
// running is picked up as soon as that probe finishes.
//
// A due subsystem whose probe is already running, from a forced refresh,
// is skipped rather than waited on (see refresh); Run re-checks it after
// busyRetry. Run returns promptly on cancellation, including while
// sleeping.
func (r *Refresher) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		now := time.Now()
		attempted := map[apiv1.Subsystem]bool{}
		for _, sub := range r.Due(now) {
			if ctx.Err() != nil {
				return
			}
			attempted[sub] = true
			r.refresh(ctx, sub, now, false)
		}

		wait, ok := r.nextWait(attempted)
		if !ok {
			// Nothing is scheduled at all.
			<-ctx.Done()
			return
		}
		timer.Reset(wait)
	}
}

// nextWait reports how long Run should sleep after a pass that attempted
// the given subsystems: until the earliest next due time, or not at all if
// a subsystem it did not attempt is already due, or busyRetry if the only
// work still due is what the pass just attempted and had to skip. It
// reports false when there are no schedules.
func (r *Refresher) nextWait(attempted map[apiv1.Subsystem]bool) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.schedules) == 0 {
		return 0, false
	}
	now := time.Now()
	var earliest time.Time
	retry := false
	for _, s := range r.schedules {
		due, ok := r.nextDue[s.Subsystem]
		if !ok || !now.Before(due) {
			if !attempted[s.Subsystem] {
				return 0, true
			}
			retry = true
			continue
		}
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	wait := earliest.Sub(now)
	if retry && (earliest.IsZero() || wait > busyRetry) {
		wait = busyRetry
	}
	return wait, true
}
