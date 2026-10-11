package logging

import (
	"fmt"
	"time"
)

// summaryEvery is how many consecutive identical failures pass before a
// summary line is emitted.
const summaryEvery = 10

// RepeatTracker collapses repeated identical failures into a first
// occurrence plus periodic summaries. The zero value is ready to use.
//
// A probe that stays broken is retried indefinitely; at a one-minute
// interval that is well over a thousand log lines a day, and that volume
// buries the events worth reading.
type RepeatTracker struct {
	last            string
	count           int
	since           time.Time
	sinceLastReport int
}

// Observe records a failure and returns the line to log, or "" to stay
// quiet. A changed message always surfaces immediately: it is a new event.
func (r *RepeatTracker) Observe(msg string, now time.Time) string {
	if msg != r.last {
		r.last = msg
		r.count = 1
		r.since = now
		r.sinceLastReport = 0
		return msg
	}

	r.count++
	r.sinceLastReport++
	if r.sinceLastReport < summaryEvery {
		return ""
	}
	r.sinceLastReport = 0
	return fmt.Sprintf("%s (still failing: %d attempts over %s)",
		msg, r.count, now.Sub(r.since).Round(time.Second))
}

// Recovered reports a return to health, once, if failures preceded it.
// Without this a suppressed failure simply goes quiet, and recovery is
// indistinguishable from continued suppression.
func (r *RepeatTracker) Recovered(now time.Time) string {
	if r.count == 0 {
		return ""
	}
	msg := fmt.Sprintf("recovered after %d failed attempts over %s",
		r.count, now.Sub(r.since).Round(time.Second))
	r.last = ""
	r.count = 0
	r.sinceLastReport = 0
	return msg
}
