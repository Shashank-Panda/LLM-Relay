package execute

import (
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// Attempt records one provider call.
//
// One per call, not one per request: a request that retried twice and then
// failed over produces three of these, and the sequence is what makes "why did
// this take four seconds" answerable after the fact. Architecture §8 defines the
// shape.
type Attempt struct {
	EndpointID string
	Provider   domain.ProviderID

	// Credential is the reference, never the secret. provider.Credential
	// carries the same distinction, which is what makes it safe for this struct
	// to end up in a log line.
	Credential string

	StartedAt time.Time
	Duration  time.Duration

	// TTFT is set on streaming attempts that produced a first token.
	//
	// It is also the pre/post-first-byte marker: an attempt with a TTFT
	// committed the client to a response, so a failure after it could not be
	// failed over (ADR-0003). Recording it is what makes the post-first-byte
	// failure rate measurable as its own number rather than lost inside the
	// general error count.
	TTFT time.Duration

	Class      provider.ErrorClass
	HTTPStatus int
	Err        error

	// Retry is which attempt this was against the same endpoint, from zero.
	Retry int

	// Backoff is how long this attempt waited before starting.
	Backoff time.Duration
}

// Failed reports whether this attempt produced an error.
func (a Attempt) Failed() bool { return a.Err != nil }

// Attempts is the sequence of calls one request made.
type Attempts []Attempt

// Last returns the final attempt, which is the one that decided the outcome.
func (a Attempts) Last() Attempt {
	if len(a) == 0 {
		return Attempt{}
	}
	return a[len(a)-1]
}

// Retries counts attempts that repeated an endpoint already tried.
func (a Attempts) Retries() int {
	n := 0
	for _, at := range a {
		if at.Retry > 0 {
			n++
		}
	}
	return n
}

// Failovers counts how many distinct endpoints were abandoned before the one
// that answered.
func (a Attempts) Failovers() int {
	seen := map[string]bool{}
	for _, at := range a {
		seen[at.EndpointID] = true
	}
	if len(seen) == 0 {
		return 0
	}
	return len(seen) - 1
}

// Endpoints lists the endpoints tried, in order, without repeats.
func (a Attempts) Endpoints() []string {
	var out []string
	for _, at := range a {
		if len(out) == 0 || out[len(out)-1] != at.EndpointID {
			out = append(out, at.EndpointID)
		}
	}
	return out
}
