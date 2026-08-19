package execute

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/Shashank-Panda/relay/internal/provider"
)

// Policy bounds how much a request may spend trying.
//
// Every field here is a spend control before it is a latency control. An
// attempt is a real charge at a real provider, so "try harder" and "cost more"
// are the same sentence — which is why none of these defaults to unlimited.
type Policy struct {
	// MaxAttempts caps total provider calls for one request, across every
	// endpoint and every retry. The route's own MaxAttempts narrows it further;
	// this is the ceiling nobody can configure past.
	MaxAttempts int

	// MaxRetriesPerEndpoint caps repeats against a single endpoint before
	// moving on. A provider that failed twice in a row is not about to succeed
	// on the third try more often than a different provider would on its first.
	//
	// Zero means the default, like every other field here. To forbid retrying
	// an endpoint while still allowing failover, set it negative; to forbid
	// both, set MaxAttempts to 1. A zero that meant "none" would be the one
	// field in this struct where leaving it blank silently changed behaviour,
	// and it would do so in the direction of failing requests that would have
	// succeeded.
	MaxRetriesPerEndpoint int

	// BaseBackoff is the first retry delay; each subsequent one doubles.
	BaseBackoff time.Duration

	// MaxBackoff caps the doubling. Without it, attempt six waits half a minute
	// inside a request somebody is watching.
	MaxBackoff time.Duration

	// AttemptTimeout bounds one call. For streaming it bounds getting the
	// stream open — never the stream itself, which is as long as the answer is.
	AttemptTimeout time.Duration

	// TotalDeadline bounds the whole request including backoff. A retry that
	// cannot finish inside what remains is not started: spending the last of
	// the budget on a call that will be cancelled mid-flight bills the customer
	// for nothing.
	TotalDeadline time.Duration

	// Rand is the jitter source, injected so tests get a deterministic
	// schedule.
	Rand func() float64

	// Sleep is the delay function, injected so tests do not wait.
	Sleep func(context.Context, time.Duration) error
}

func DefaultPolicy() Policy {
	return Policy{
		// Four calls is enough for two endpoints with one retry each. Past that
		// the request is usually doomed and the spend is not.
		MaxAttempts:           4,
		MaxRetriesPerEndpoint: 2,
		BaseBackoff:           200 * time.Millisecond,
		MaxBackoff:            5 * time.Second,
		// Sixty seconds accommodates a long reasoning response's headers. It is
		// not the generation budget — that is the request context.
		AttemptTimeout: 60 * time.Second,
		TotalDeadline:  120 * time.Second,
		Rand:           rand.Float64,
		Sleep:          sleepCtx,
	}
}

func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = d.MaxAttempts
	}
	switch {
	case p.MaxRetriesPerEndpoint == 0:
		p.MaxRetriesPerEndpoint = d.MaxRetriesPerEndpoint
	case p.MaxRetriesPerEndpoint < 0:
		p.MaxRetriesPerEndpoint = 0
	}
	if p.BaseBackoff <= 0 {
		p.BaseBackoff = d.BaseBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = d.MaxBackoff
	}
	if p.AttemptTimeout <= 0 {
		p.AttemptTimeout = d.AttemptTimeout
	}
	if p.TotalDeadline <= 0 {
		p.TotalDeadline = d.TotalDeadline
	}
	if p.Rand == nil {
		p.Rand = d.Rand
	}
	if p.Sleep == nil {
		p.Sleep = d.Sleep
	}
	return p
}

// backoff computes the wait before a retry.
//
// Exponential with full jitter: the delay is a uniform draw from [0, 2^n × base]
// rather than the deterministic 2^n × base. The deterministic form synchronizes
// every client that failed at the same moment into retrying at the same moment,
// which is how a provider's brief hiccup becomes a sustained outage — the
// thundering herd re-forms on every subsequent attempt. Full jitter spreads them
// and, unintuitively, also finishes sooner in aggregate.
//
// A provider-supplied Retry-After wins outright. It is a number the provider
// actually knows and Relay is guessing at, and a gateway that substitutes its
// own backoff for a stated one is guessing against the truth.
func (p Policy) backoff(attempt int, err error) time.Duration {
	if after := retryAfter(err); after > 0 {
		if after > p.MaxBackoff {
			return p.MaxBackoff
		}
		return after
	}

	d := p.BaseBackoff << attempt
	if d > p.MaxBackoff || d <= 0 { // d <= 0 catches the shift overflowing
		d = p.MaxBackoff
	}
	return time.Duration(p.Rand() * float64(d))
}

func retryAfter(err error) time.Duration {
	var pe *provider.Error
	if ok := asProviderError(err, &pe); ok {
		return pe.RetryAfter
	}
	return 0
}

// sleepCtx waits, or returns early when the request is cancelled.
//
// A plain time.Sleep in a retry loop holds a goroutine for its full duration
// after the client has already hung up, which is how a gateway accumulates
// goroutines during exactly the incident that produced the retries.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
