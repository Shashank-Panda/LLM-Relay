// Package execute performs the provider call a Decision selected, and keeps
// trying until it has an answer or a reason to stop.
//
// Everything here is arranged around one asymmetry: a retry costs money and a
// failure costs a request. That is why the error class decides the action rather
// than the HTTP status, why Terminal is the default for anything unrecognised,
// and why every bound in Policy is a spend control before it is a latency one.
//
// The boundary that shapes the streaming path is ADR-0003: failover is permitted
// only before the first byte reaches the client. Once a chunk is flushed the
// client has committed — it has parsed content and probably rendered it — and
// there is no honest way to switch providers and continue. So the streaming
// attempt loop ends the moment a stream is successfully opened, and the server
// owns everything after that.
package execute

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/health"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/routing"
)

// Executor turns a decision into a provider call.
type Executor struct {
	registry *provider.Registry
	resolver provider.Resolver
	policy   Policy

	// tracker records what happened, so the router can stop selecting endpoints
	// that are failing. Nil disables circuit breaking entirely, which is the
	// Phase 1 behaviour and still works.
	tracker *health.Tracker

	// onDegraded is called each time a fail-open path is taken. Passthrough is
	// silent by design, so without this a gateway can be recovering from
	// failures on every request and look perfectly healthy.
	onDegraded func(component, reason string)

	// onAttempt is called once per provider call, before the request finishes.
	//
	// It exists because per-attempt facts cannot be recovered from the ledger
	// record: a record carries the endpoint that answered, so attributing a
	// retry to it would label the failures of a broken endpoint with the name
	// of the healthy one that rescued the request — pointing an operator at
	// exactly the wrong provider during an incident.
	onAttempt func(Attempt)
}

func New(registry *provider.Registry, resolver provider.Resolver) *Executor {
	return &Executor{registry: registry, resolver: resolver, policy: DefaultPolicy()}
}

// WithPolicy replaces the retry and deadline bounds.
func (e *Executor) WithPolicy(p Policy) *Executor {
	c := *e
	c.policy = p.withDefaults()
	return &c
}

// WithTracker attaches circuit breaking and latency observation.
func (e *Executor) WithTracker(t *health.Tracker) *Executor {
	c := *e
	c.tracker = t
	return &c
}

// WithDegradedHook registers the fail-open counter.
func (e *Executor) WithDegradedHook(fn func(component, reason string)) *Executor {
	c := *e
	c.onDegraded = fn
	return &c
}

// WithAttemptHook registers a per-attempt observer.
func (e *Executor) WithAttemptHook(fn func(Attempt)) *Executor {
	c := *e
	c.onAttempt = fn
	return &c
}

func (e *Executor) observe(a Attempt) {
	if e.onAttempt != nil {
		e.onAttempt(a)
	}
}

func (e *Executor) degraded(component, reason string) {
	if e.onDegraded != nil {
		e.onDegraded(component, reason)
	}
}

// prepare resolves the endpoint, adapter, and credential for one candidate.
//
// Failures here are Relay's own, not the provider's, and they are classified
// Terminal for that endpoint: a missing adapter or an unconfigured credential
// will fail exactly the same way on every retry, so spending attempts on them
// burns the request deadline before failing anyway. They do not stop the loop —
// the *next* candidate may well be configured — which is why this returns an
// error the caller treats as RetryOther rather than one it surfaces.
func (e *Executor) prepare(cat *domain.Catalog, endpointID string) (*domain.ModelEndpoint, provider.Adapter, provider.Credential, error) {
	ep, ok := cat.Endpoint(endpointID)
	if !ok {
		return nil, nil, provider.Credential{}, &provider.Error{
			Endpoint: endpointID,
			Class:    provider.ClassRetryOther,
			Message:  fmt.Sprintf("endpoint %q is not in catalog %s", endpointID, cat.Version),
		}
	}

	adapter, err := e.registry.For(ep.Provider)
	if err != nil {
		return nil, nil, provider.Credential{}, &provider.Error{
			Provider: string(ep.Provider), Endpoint: ep.ID,
			Class: provider.ClassRetryOther, Message: err.Error(),
		}
	}

	cred, err := e.resolver.Resolve(ep.CredentialRef)
	if err != nil {
		// The resolver's message names the missing configuration, never the
		// secret. That is a property of provider.ErrNoCredential, not of care
		// taken here.
		return nil, nil, provider.Credential{}, &provider.Error{
			Provider: string(ep.Provider), Endpoint: ep.ID,
			Class: provider.ClassRetryOther, Message: err.Error(),
		}
	}

	return ep, adapter, cred, nil
}

// Result is what one execution produced.
type Result struct {
	// Response is set for a completed non-streaming call.
	Response *provider.Response

	// Stream is set for a successfully opened streaming call. The caller owns
	// it and must Close it on every path.
	Stream provider.Stream

	Endpoint *domain.ModelEndpoint
	Attempts Attempts

	// Rerouted records that a Reroute class sent the request back through the
	// router with a corrected constraint.
	Rerouted bool

	// Escalation is set when a downgraded endpoint produced invalid output and
	// the baseline was retried (ADR-0009). Nil on every ordinary request.
	Escalation *Escalation

	// Discarded is the usage of attempts that were paid for and thrown away.
	// Empty unless something escalated.
	Discarded []Charge
}

// Chat performs a non-streaming completion, retrying and failing over.
//
// Non-streaming is the easy case for failover: the whole response is buffered
// inside the adapter, so nothing has reached the client and any attempt may be
// abandoned right up to the point one succeeds.
func (e *Executor) Chat(
	ctx context.Context,
	req *domain.NormalizedRequest,
	cat *domain.Catalog,
	d *domain.Decision,
) (*provider.Response, *domain.ModelEndpoint, Attempt, error) {
	res, err := e.Run(ctx, Request{Request: req, Catalog: cat, Decision: d})
	return res.Response, res.Endpoint, res.Attempts.Last(), err
}

// Stream opens a streaming completion, retrying and failing over up to the
// first byte.
func (e *Executor) Stream(
	ctx context.Context,
	req *domain.NormalizedRequest,
	cat *domain.Catalog,
	d *domain.Decision,
) (provider.Stream, *domain.ModelEndpoint, Attempt, error) {
	res, err := e.Run(ctx, Request{Request: req, Catalog: cat, Decision: d, Streaming: true})
	return res.Stream, res.Endpoint, res.Attempts.Last(), err
}

// Request is one execution.
type Request struct {
	Request  *domain.NormalizedRequest
	Catalog  *domain.Catalog
	Decision *domain.Decision

	Streaming bool

	// Policy overrides the executor's defaults for this request. Zero fields
	// fall back.
	Policy *Policy

	// Reroute re-runs the router with a corrected constraint. Nil disables
	// Reroute handling, which then degrades to RetryOther — the next candidate
	// in the existing ranking, which is a worse answer than re-filtering but a
	// better one than failing.
	Reroute func(corrected *domain.NormalizedRequest) (*domain.Decision, error)
}

// Run executes a request against its decision's ranked candidates.
//
// The loop is: for each candidate in ranked order, try it, retrying the same
// endpoint on RetrySame with backoff, moving to the next on RetryOther,
// re-routing on Reroute, and stopping outright on Terminal or Cancelled. Every
// attempt is recorded, and every bound in Policy applies across the whole loop
// rather than per candidate.
func (e *Executor) Run(ctx context.Context, in Request) (Result, error) {
	pol := e.policy
	if in.Policy != nil {
		pol = in.Policy.withDefaults()
	}

	// The total deadline covers backoff as well as calls. Derived from the
	// inbound context, so a client disconnect still cancels everything — this
	// shortens the budget, it never extends it.
	//
	// Not a plain defer cancel(). On a streaming request Run returns as soon as
	// the stream is open, and cancelling here would kill the stream it just
	// produced. The deadline bounds getting an answer started; how long the
	// answer takes to arrive is the answer's business, so on success the
	// stream adopts this context and cancels it when the caller closes.
	inner, rel := bounded(ctx, pol.TotalDeadline)

	st := &run{exec: e, in: in, pol: pol}
	res, err := st.loop(inner)

	if ms, ok := res.Stream.(*managedStream); ok && err == nil {
		ms.adopt(rel)
	} else {
		rel.close()
	}
	return res, err
}

// run is one execution's mutable state, kept off the Executor so a single
// Executor stays safe for concurrent use.
type run struct {
	exec *Executor
	in   Request
	pol  Policy

	attempts  Attempts
	rerouted  bool
	escalated bool
	discarded []Charge

	// tried remembers endpoints already exhausted, so a reroute that returns a
	// ranking containing them does not start over on one that just failed.
	tried map[string]bool
}

func (r *run) loop(ctx context.Context) (Result, error) {
	r.tried = map[string]bool{}

	var lastErr error

	for {
		candidates := r.candidates()
		progressed := false

		for _, id := range candidates {
			if r.tried[id] {
				continue
			}
			progressed = true

			res, err, action := r.tryEndpoint(ctx, id)
			switch action {
			case actionDone:
				// The answer exists; the remaining question is whether it is
				// usable. A downgrade that returns invalid output has not saved
				// anything, and escalating is what stops an over-optimistic
				// catalog score from being silently harmful.
				return r.escalate(ctx, res)
			case actionStop:
				return r.result(), err
			case actionReroute:
				if r.reroute(id) {
					lastErr = err
					// Break out to the outer loop, which reads the new ranking.
					goto next
				}
				// No rerouter, or rerouting failed. Degrade to trying the next
				// candidate in the ranking that already exists.
				r.exec.degraded("executor", "reroute_unavailable")
				lastErr = err
			case actionNext:
				lastErr = err
			}
			r.tried[id] = true

			if len(r.attempts) >= r.pol.MaxAttempts {
				return r.result(), r.exhausted(lastErr, "attempt budget spent")
			}
			if ctx.Err() != nil {
				return r.result(), r.deadline(ctx, lastErr)
			}
		}

		if !progressed {
			// Every candidate in the current ranking has been tried. Nothing
			// left to escalate to.
			return r.result(), r.exhausted(lastErr, "every candidate was tried")
		}
	next:
	}
}

type action int

const (
	actionDone action = iota
	actionNext
	actionStop
	actionReroute
)

// tryEndpoint runs one endpoint's attempts, including its retries.
func (r *run) tryEndpoint(ctx context.Context, id string) (Result, error, action) {
	ep, adapter, cred, err := r.exec.prepare(r.in.Catalog, id)
	if err != nil {
		r.attempts = append(r.attempts, Attempt{
			EndpointID: id, Class: provider.ClassOf(err), Err: err,
		})
		return Result{}, err, actionNext
	}

	key := health.Key{Endpoint: ep.ID, Credential: ep.CredentialRef}

	var lastErr error
	for retry := 0; ; retry++ {
		if len(r.attempts) >= r.pol.MaxAttempts {
			return Result{}, lastErr, actionNext
		}

		// The breaker is consulted here as well as during routing, and the two
		// are not redundant. Routing filtered on a snapshot taken microseconds
		// ago; this admits the half-open probe and refuses the request behind
		// it, which is the difference between finding out whether an endpoint
		// recovered and finding out by sending it everything at once.
		if !r.exec.tracker.Allow(key) {
			r.attempts = append(r.attempts, Attempt{
				EndpointID: ep.ID, Provider: ep.Provider, Credential: ep.CredentialRef,
				Retry: retry, Class: provider.ClassRetryOther,
				Err: &provider.Error{
					Provider: string(ep.Provider), Endpoint: ep.ID,
					Class:   provider.ClassRetryOther,
					Message: "circuit breaker is open for this endpoint",
				},
			})
			return Result{}, r.attempts.Last().Err, actionNext
		}

		var backoff time.Duration
		if retry > 0 {
			backoff = r.pol.backoff(retry-1, lastErr)
			if !r.affords(ctx, backoff) {
				// Not enough of the deadline left to wait and then call. Move
				// on rather than burn the remainder on a call that will be
				// cancelled mid-flight and billed anyway.
				return Result{}, lastErr, actionNext
			}
			if err := r.pol.Sleep(ctx, backoff); err != nil {
				return Result{}, err, actionStop
			}
		}

		res, err := r.call(ctx, ep, adapter, cred, retry, backoff)
		if err == nil {
			return res, nil, actionDone
		}
		lastErr = err

		switch provider.ClassOf(err) {
		case provider.ClassRetrySame:
			if retry >= r.pol.MaxRetriesPerEndpoint {
				// Out of patience with this endpoint. A different one is more
				// likely to work than a third try at the same one.
				return Result{}, err, actionNext
			}
			continue
		case provider.ClassRetryOther:
			return Result{}, err, actionNext
		case provider.ClassReroute:
			return Result{}, err, actionReroute
		default:
			// Terminal and Cancelled. A malformed request fails identically
			// everywhere — retrying it across three providers produces three
			// bills and one guaranteed failure — and nobody is waiting for a
			// cancelled one.
			return Result{}, err, actionStop
		}
	}
}

// call performs a single provider call and records it.
func (r *run) call(
	ctx context.Context,
	ep *domain.ModelEndpoint,
	adapter provider.Adapter,
	cred provider.Credential,
	retry int,
	backoff time.Duration,
) (Result, error) {
	att := Attempt{
		EndpointID: ep.ID,
		Provider:   ep.Provider,
		Credential: ep.CredentialRef,
		StartedAt:  time.Now(),
		Retry:      retry,
		Backoff:    backoff,
	}

	attemptCtx, rel := r.attemptContext(ctx)

	var (
		res Result
		err error
	)
	if r.in.Streaming {
		var st provider.Stream
		st, err = adapter.ChatStream(attemptCtx, r.in.Request, ep, cred)
		if err == nil {
			// The attempt timeout has done its job — the stream is open. Stop
			// the timer *without* cancelling, so the stream lives as long as
			// the answer does and still dies with the request context. A plain
			// context.WithTimeout here would terminate a working response
			// mid-generation at the sixty second mark.
			rel.disarm()
			res = Result{Stream: &managedStream{Stream: st, releases: []*release{rel}}, Endpoint: ep}
		}
	} else {
		var resp *provider.Response
		resp, err = adapter.Chat(attemptCtx, r.in.Request, ep, cred)
		if err == nil {
			res = Result{Response: resp, Endpoint: ep}
		}
	}

	att.Duration = time.Since(att.StartedAt)

	if err != nil {
		rel.close()
		att.Class = provider.ClassOf(err)
		att.HTTPStatus = provider.HTTPStatusOf(err)
		att.Err = err
		r.attempts = append(r.attempts, att)
		r.exec.observe(att)
		r.exec.tracker.Observe(
			health.Key{Endpoint: ep.ID, Credential: ep.CredentialRef}, att.Class, 0)
		return Result{}, err
	}

	r.attempts = append(r.attempts, att)
	r.exec.observe(att)
	res.Attempts = r.attempts
	res.Rerouted = r.rerouted

	// Latency is observed only for the non-streaming case here. A stream's
	// duration at this point is the time to open it, which is dominated by
	// connection setup rather than by the model — the meaningful number is
	// time-to-first-token, and the server reports it once it has one.
	var latency time.Duration
	if !r.in.Streaming {
		latency = att.Duration
	}
	r.exec.tracker.Observe(
		health.Key{Endpoint: ep.ID, Credential: ep.CredentialRef}, "", latency)

	return res, nil
}

// reroute re-runs the router with the failing endpoint's limit taken as fact.
//
// This is what makes Reroute worth having as its own class. A context overflow
// is neither transient nor permanent: it is a statement that the constraint set
// used for routing was wrong. Retrying is pointless and failing is premature —
// the right move is to correct the constraint and re-filter, which eliminates
// the endpoint that just refused *and* every endpoint too small to do better.
func (r *run) reroute(failed string) bool {
	if r.in.Reroute == nil || r.rerouted {
		// One reroute per request. A second would mean the corrected constraint
		// was also wrong, and the loop that follows from believing otherwise is
		// unbounded.
		return false
	}

	corrected := r.correctedRequest(failed)
	d, err := r.in.Reroute(corrected)
	if err != nil || d == nil || d.Chosen == "" {
		return false
	}

	r.in.Request = corrected
	r.in.Decision = d
	r.rerouted = true
	return true
}

// correctedRequest raises the estimate past what the failing endpoint could
// hold.
//
// The estimate was an approximation — four bytes per token over text that may
// not be English — and the provider has just supplied a measurement that
// contradicts it. Taking the failing endpoint's own context window as the new
// floor is the smallest correction consistent with what was observed: it
// eliminates that endpoint and everything no larger, without inventing a number
// nobody measured.
func (r *run) correctedRequest(failed string) *domain.NormalizedRequest {
	out := r.in.Request.Clone()
	ep, ok := r.in.Catalog.Endpoint(failed)
	if !ok {
		return out
	}

	need := ep.Limits.ContextWindow + 1
	have := out.Estimate.InputTokens + out.Estimate.MaxOutputTokens
	if have >= need {
		// The estimate already exceeded the window, so the filter should have
		// eliminated this endpoint and did not. Nothing to correct here; the
		// re-filter will produce the same ranking and the caller falls through
		// to the next candidate.
		return out
	}
	out.Estimate.InputTokens += need - have
	return out
}

// candidates is the endpoint order to try, best first.
func (r *run) candidates() []string {
	d := r.in.Decision
	if d == nil {
		return nil
	}

	out := make([]string, 0, len(d.Failover)+1)
	// Chosen first, then whatever the router decided may replace it.
	//
	// Deliberately not "the rest of Ranked". In shadow and strict mode the served
	// endpoint is the baseline while Ranked holds the counterfactual ordering, so
	// walking the ranking would fail over to the endpoint the tenant declined to
	// be served — turning a provider hiccup into the substitution they refused.
	// The router computes the permitted set per mode; this consumes it.
	if d.Chosen != "" {
		out = append(out, d.Chosen)
	}
	for _, id := range d.Failover {
		if id != d.Chosen {
			out = append(out, id)
		}
	}
	return out
}

// affords reports whether the deadline leaves room to wait and then call.
func (r *run) affords(ctx context.Context, backoff time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	// A minimum viable call is not zero. Requiring only that the backoff fits
	// would start a call with microseconds left, which is a charge for a
	// response nobody receives.
	const minCall = 250 * time.Millisecond
	return time.Until(deadline) > backoff+minCall
}

func (r *run) result() Result {
	return Result{Attempts: r.attempts, Rerouted: r.rerouted}
}

// exhausted wraps the last failure with why the loop stopped.
func (r *run) exhausted(last error, why string) error {
	if last == nil {
		last = &provider.Error{Class: provider.ClassRetryOther, Message: why}
	}
	return fmt.Errorf("%s after %d attempt(s): %w", why, len(r.attempts), last)
}

// deadline reports a request that ran out of time.
func (r *run) deadline(ctx context.Context, last error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		// The client left. Not Relay's failure and not the provider's, and
		// counting it as one would put every abandoned request into the error
		// budget.
		return &provider.Error{Class: provider.ClassCancelled, Message: "client cancelled", Err: ctx.Err()}
	}
	return &provider.Error{
		Class:   provider.ClassRetryOther,
		Message: fmt.Sprintf("request deadline exceeded after %d attempt(s)", len(r.attempts)),
		Err:     errors.Join(ctx.Err(), last),
	}
}

// Rerouter builds the callback Run uses for the Reroute class.
//
// It lives here rather than in the gateway so that the correction and the
// re-filter stay next to each other: they are two halves of one behaviour, and
// splitting them across packages is how the second half stops matching the
// first.
func Rerouter(cat *domain.Catalog, pol *domain.Policy) func(*domain.NormalizedRequest) (*domain.Decision, error) {
	return func(corrected *domain.NormalizedRequest) (*domain.Decision, error) {
		return routing.Route(routing.Input{
			Request: corrected.RoutingView(),
			Catalog: cat,
			Policy:  pol,
		})
	}
}

func asProviderError(err error, out **provider.Error) bool {
	return errors.As(err, out)
}
