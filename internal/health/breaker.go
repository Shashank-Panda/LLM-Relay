package health

import "time"

// event is one observation in the rolling window.
//
// Timestamps rather than fixed buckets. Buckets are cheaper and are the usual
// choice, but they quantize the window boundary: with ten buckets over thirty
// seconds, a burst of failures can be counted for anywhere between 27 and 33
// seconds depending on when it landed, and a breaker whose behaviour depends on
// arrival phase is one nobody can reason about from a test. At the volume one
// breaker sees within one window this slice stays small, and trim keeps it so.
type event struct {
	at     time.Time
	failed bool
}

// breaker is one (endpoint, credential) pair's state machine.
//
// Not safe for concurrent use on its own; the Tracker's mutex covers it. A lock
// per breaker would be finer-grained and would buy nothing — every operation
// here is a slice append and a comparison.
type breaker struct {
	events []event

	// openedAt is when the breaker last tripped. Zero means it is not open.
	openedAt time.Time

	// probes counts attempts admitted since half-open began, so the quota is
	// enforced against admissions rather than against completions. Counting
	// completions would admit an unbounded number of concurrent probes while
	// the first one is still in flight, which is the stampede the quota exists
	// to prevent.
	probes int
}

// state derives the current position without mutating anything.
//
// Derived rather than stored, because the open→half-open transition is driven
// by the passage of time and nothing else. A stored state would need a timer or
// a sweeper to advance it, and both are machinery for a value that a
// subtraction computes exactly.
func (b *breaker) state(cfg Config, now time.Time) State {
	if b.openedAt.IsZero() {
		return StateClosed
	}
	if now.Sub(b.openedAt) < cfg.OpenFor {
		return StateOpen
	}
	if b.probes >= cfg.HalfOpenProbes {
		// The probe quota is spent and none of them has resolved yet. Report
		// open: routing must keep steering away until an answer arrives.
		return StateOpen
	}
	return StateHalfOpen
}

// allow admits an attempt, consuming a probe if the breaker is testing.
func (b *breaker) allow(cfg Config, now time.Time) bool {
	switch b.state(cfg, now) {
	case StateClosed:
		return true
	case StateHalfOpen:
		b.probes++
		return true
	default:
		return false
	}
}

func (b *breaker) succeed(cfg Config, now time.Time) {
	if !b.openedAt.IsZero() {
		// A probe came back clean. Reset completely rather than merely closing:
		// the window still holds the failures that tripped it, and carrying
		// them forward would re-trip the breaker on the next single failure
		// and leave the endpoint flapping.
		b.reset()
		return
	}
	b.record(cfg, now, false)
}

func (b *breaker) fail(cfg Config, now time.Time) {
	if !b.openedAt.IsZero() {
		// A probe failed. Restart the cool-down from now, so a persistently
		// broken endpoint is retried on a fixed interval rather than
		// continuously.
		b.openedAt = now
		b.probes = 0
		return
	}

	b.record(cfg, now, true)

	reqs, fails := b.counts()
	if reqs < cfg.MinRequests {
		// Not enough evidence. Two failures out of two is a 100% error rate and
		// is indistinguishable from bad luck.
		return
	}
	if float64(fails)/float64(reqs) >= cfg.FailureRatio {
		b.openedAt = now
		b.probes = 0
	}
}

// ignore advances the window without counting the attempt either way.
//
// Terminal and Cancelled outcomes land here. They are real events but they are
// not evidence about the endpoint: a malformed request fails identically
// everywhere, and a client hanging up says nothing about the provider. Counting
// them as successes would be just as wrong as counting them as failures — it
// would let a flood of 400s hold a genuinely broken endpoint's error ratio down
// below the threshold.
func (b *breaker) ignore(cfg Config, now time.Time) {
	b.trim(cfg, now)
}

func (b *breaker) record(cfg Config, now time.Time, failed bool) {
	b.trim(cfg, now)
	b.events = append(b.events, event{at: now, failed: failed})
}

// trim drops events that have fallen out of the window.
func (b *breaker) trim(cfg Config, now time.Time) {
	cutoff := now.Add(-cfg.Window)
	i := 0
	for i < len(b.events) && !b.events[i].at.After(cutoff) {
		i++
	}
	if i == 0 {
		return
	}
	// Copy down rather than reslice. Resliced backing arrays only ever grow,
	// and this one is appended to for the life of the process.
	b.events = append(b.events[:0], b.events[i:]...)
}

func (b *breaker) counts() (requests, failures int) {
	for _, e := range b.events {
		requests++
		if e.failed {
			failures++
		}
	}
	return requests, failures
}

func (b *breaker) reset() {
	b.events = b.events[:0]
	b.openedAt = time.Time{}
	b.probes = 0
}
