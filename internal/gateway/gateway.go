package gateway

import (
	"context"
	"time"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	healthpkg "github.com/Shashank-Panda/relay/internal/health"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/optimize"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/respcache"
	"github.com/Shashank-Panda/relay/internal/routing"
	"github.com/Shashank-Panda/relay/internal/tenant"
)

// Gateway wires the pipeline stages together.
type Gateway struct {
	Store    *catalog.Store
	Executor *execute.Executor

	// Tenants resolves an API key to a tenant and its policy. Nil means every
	// request is anonymous and runs under the default policy, which is Phase 1's
	// behaviour and is still supported.
	Tenants *tenant.Registry

	// Policy is the fallback when no tenant registry is configured.
	Policy *domain.Policy

	// Stats is the observed per-route history the output-ceiling lever reads.
	// Nil means no history, which makes that lever decline to act — the correct
	// behaviour for a process that has not seen enough traffic to have an
	// opinion.
	Stats optimize.StatsSource

	// Cache is the exact-match response cache. Nil disables it entirely,
	// regardless of what any route says.
	Cache *respcache.Store

	// Health is the circuit-breaker and latency view. Nil means every endpoint
	// is treated as available with unknown latency, which is Phase 1's
	// behaviour and is the fail-open direction: an absent health signal must
	// mean "route normally", never "refuse to route" (ADR-0010).
	Health *healthpkg.Tracker

	// Credentials answers which credential refs are usable, so the router can
	// eliminate an endpoint the caller has no key for instead of ranking it and
	// letting the executor discover the same thing one paid attempt later.
	//
	// Nil means "do not filter on credentials" — the behaviour every deployment
	// had before this existed, and the fail-open direction (ADR-0010).
	Credentials provider.Resolver

	// OnDegraded counts each fail-open path taken. Nil is safe and is how the
	// degradations become invisible, which is the specific failure ADR-0010
	// exists to prevent — so production wires it and tests may not.
	OnDegraded func(component, reason string)
}

// CredentialSnapshot reports which of the catalog's credential refs can be
// resolved right now.
//
// Called before Prepare rather than inside it, and that placement is the point:
// Prepare documents itself as performing no I/O, and a credential store — which
// is what a hosted deployment resolves against — makes that a real query. Taking
// the snapshot outside keeps both Prepare and routing.Route pure functions over
// their arguments, and puts the one piece of latency somewhere an operator can
// find it.
//
// extra is the request's own credentials, tried ahead of the gateway's. It is
// nil today and is the seam BYOK arrives through.
//
// Returns nil — meaning "no opinion", which eliminates nothing — when there is
// nothing to ask. Refusing to route because credential availability is unknown
// would be failing closed on our own telemetry.
func (g *Gateway) CredentialSnapshot(ctx context.Context, cat *domain.Catalog, tenantID string, extra provider.Resolver) *domain.CredentialSet {
	if cat == nil || (g.Credentials == nil && extra == nil) {
		return nil
	}
	refs := cat.CredentialRefs()
	if len(refs) == 0 {
		return nil
	}

	var available []string
	if extra != nil {
		available = provider.AvailableRefs(ctx, extra, tenantID, refs)
	}
	if g.Credentials != nil {
		have := make(map[string]bool, len(available))
		for _, r := range available {
			have[r] = true
		}
		for _, r := range provider.AvailableRefs(ctx, g.Credentials, tenantID, refs) {
			if !have[r] {
				available = append(available, r)
			}
		}
	}
	return domain.NewCredentialSet(available...)
}

func (g *Gateway) degraded(component, reason string) {
	if g.OnDegraded != nil {
		g.OnDegraded(component, reason)
	}
}

// Compile-time proof that the metering histogram satisfies what the optimizer
// asks for. The assertion lives here rather than in meter because this is the
// package that depends on both, and meter must not import optimize.
var _ optimize.StatsSource = (*meter.RouteStats)(nil)

// policyFor returns the policy a request runs under.
//
// Per-tenant rather than process-wide, because the adoption story is one
// customer trying shadow mode while everybody else stays strict. A global flag
// would make that impossible without a second deployment.
func (g *Gateway) policyFor(t *tenant.Tenant) *domain.Policy {
	if t != nil && t.Policy != nil {
		return t.Policy
	}
	if g.Policy != nil {
		return g.Policy
	}
	return domain.DefaultPolicy()
}

// Prepared is a request that has been routed but not yet executed.
//
// Separating preparation from execution is what lets the streaming and
// non-streaming handlers share every decision-making stage while diverging only
// at the provider call — and it is what makes the decision available to write
// disclosure headers *before* a stream has started, which after the first byte
// is impossible.
type Prepared struct {
	Request  *domain.NormalizedRequest
	Catalog  *domain.Catalog
	Decision *domain.Decision

	// Original is the request as the caller sent it, before the optimizer.
	//
	// Kept because "what did the customer actually ask for" must stay
	// answerable after the fact. Dry-run reports the before/after token counts
	// from it, and Phase 4's failover will need it to retry an unoptimized
	// request when an optimized one is rejected.
	Original *domain.NormalizedRequest

	// Optimize is how the optimization pass went, including the passes that
	// applied nothing and the ones that failed open.
	Optimize optimize.Result

	// RequestedModel is what the caller put in the model field. Echoed back
	// verbatim, because clients key caches and dashboards on it.
	RequestedModel string

	// Tenant owns this request and its costs.
	Tenant *tenant.Tenant

	// Route is the resolved route, or nil when the caller pinned a model that
	// no route declares as its baseline.
	Route *domain.Route

	// Policy is the tenant policy this request ran under. Held because a
	// Reroute has to re-filter with the same policy the first pass used —
	// re-deriving it mid-request would let a config reload change the rules
	// halfway through one decision.
	Policy *domain.Policy

	// Attempts is every provider call this request made, in order. Empty until
	// execution.
	Attempts execute.Attempts

	// Rerouted records that a provider rejected the request on a constraint the
	// router had wrong, and the router was re-run with it corrected.
	Rerouted bool

	// Escalation is set when a downgrade produced invalid output and the
	// baseline was retried; Discarded is what the abandoned attempt cost.
	Escalation *execute.Escalation
	Discarded  []execute.Charge

	// PinnedStrict records an explicit X-Relay-Pin: strict on this request.
	//
	// Distinct from a tenant whose mode is strict, and the distinction is
	// load-bearing. Tenant strict mode means "do not serve me a different
	// model"; it does not forbid rewriting the request sent to the model they
	// asked for, which is the whole reason the levers ship before routing
	// (ADR-0008). The pin header is the stronger statement — the escape hatch
	// for the one request where nothing may be touched — so it disables the
	// optimizer and the response cache as well. An escape hatch with exceptions
	// is not one.
	PinnedStrict bool

	// CacheKey is non-empty when this request may read from and write to the
	// response cache. Computed once, in Prepare, so the lookup, the store, and
	// the dry-run explanation cannot disagree about it.
	CacheKey string

	// CacheSkip explains an empty CacheKey, for dry-run and for the operator
	// asking why nothing is being cached.
	CacheSkip respcache.Reason

	// CacheHit is set by Chat or Stream when the answer came from the cache.
	CacheHit bool

	// CachedUsage is the stored usage of a cache hit: what this request would
	// have consumed. It is what makes the saving computable, since no provider
	// reported anything this time.
	CachedUsage provider.Usage

	// Credentials is what this caller can actually resolve — held for the
	// dry-run explanation, and reported even when routing was told to ignore it.
	// Refs only; this never holds a secret.
	Credentials *domain.CredentialSet

	// requestCreds is what the caller supplied on this request, as opposed to
	// the deployment's own. Unexported: it holds live provider keys, and the
	// only things that may read it are the executor, which needs to make the
	// call, and CacheScope, which needs to isolate on it.
	requestCreds *provider.KeySet

	// AssumedCredentials records that the ranking above was computed as though
	// every credential existed. True only on a dry run, and the explanation has
	// to say so: a ranking that assumed keys the reader does not hold is a
	// different claim from one that did not.
	AssumedCredentials bool
}

// Prepare optimizes the request and routes it.
//
// No I/O and no provider contact: everything here is a pure function over a
// catalog snapshot, plus one bounded read of the route-stats histogram. The
// snapshot is taken once, at the top, so a request that begins under catalog
// version N completes under version N even if a reload lands mid-flight.
//
// The stage order is fixed and each step needs the one before it. The baseline
// resolves the route name; the optimizer needs the route name to look up that
// route's observed output lengths; and the router must see the request as it
// will actually be sent, because an optimizer that lowers max_tokens changes
// which endpoints the request fits in. Routing against the pre-optimization
// request would eliminate candidates the optimizer had just made viable.
// PrepareOptions carries the per-request inputs that are not the request.
//
// A struct rather than more positional parameters: Prepare was already at four
// strings, and the next two additions are a boolean and a credential set, which
// is precisely the shape that produces a call nobody can read and an argument
// swap nobody notices.
type PrepareOptions struct {
	// Model is the caller's requested model, echoed back verbatim.
	Model string

	// Pin is the X-Relay-Pin header. strict and shadow are honoured; optimize
	// is not, because permission to substitute belongs to the tenant.
	Pin string

	// Credentials is which credential refs this request can use. Nil means "do
	// not filter on credentials", which is the behaviour every deployment had
	// before this existed.
	Credentials *domain.CredentialSet

	// RequestCredentials is what the caller supplied on this request, if
	// anything. It is tried ahead of the deployment's own credentials, and its
	// presence narrows the response cache's isolation scope — see CacheScope,
	// where the reason is that under caller-supplied keys the tenant stops being
	// the security boundary.
	RequestCredentials *provider.KeySet

	// AssumeCredentials routes as though every credential were available.
	//
	// This exists for one caller — a dry run — and must never be set on a live
	// request. Routing to an endpoint Relay cannot authenticate to would turn an
	// explanation feature into an outage. The HTTP layer gates it; this field
	// only carries the decision.
	AssumeCredentials bool
}

func (g *Gateway) Prepare(req *domain.NormalizedRequest, tn *tenant.Tenant, opts PrepareOptions) (*Prepared, error) {
	model, pin := opts.Model, opts.Pin
	cat := g.Store.Current()
	if cat == nil {
		return nil, &ErrUnknownModel{Model: model}
	}

	pol := g.policyFor(tn)
	mode := ResolveMode(pin, pol)
	pinned := domain.BaselineMode(pin) == domain.ModeStrict

	// A snapshot past its maximum age degrades to serving what was asked for.
	//
	// This is the one fail-open path that gets *more* conservative rather than
	// less. Everywhere else, degrading means doing less work and serving
	// anyway; here it means declining to make cost and quality decisions from
	// prices nobody has confirmed recently. A stale catalog does not error — it
	// routes, confidently, on numbers that stopped being true, and every
	// savings figure computed against them is wrong in a way that looks
	// entirely plausible. ADR-0010 names this the sharpest edge of failing open.
	stale := g.Store.Stale()
	if stale && mode != domain.ModeStrict {
		g.degraded("catalog", "stale_snapshot")
		mode = domain.ModeStrict
	}

	if tn != nil {
		req.Tenant = tn.ID
	}

	routeName, baseline, err := ResolveBaseline(cat, model, mode)
	if err != nil {
		return nil, err
	}

	req.RouteName = routeName
	req.Baseline = baseline

	p := &Prepared{
		Original:       req,
		Request:        req,
		Catalog:        cat,
		RequestedModel: model,
		Tenant:         tn,
		PinnedStrict:   pinned,
		Policy:         pol,
		requestCreds:   opts.RequestCredentials,
	}
	if rt, ok := cat.Route(routeName); ok {
		p.Route = rt
	}

	if !pinned {
		p.Optimize = optimize.New(pol.Levers).Run(req, g.Stats)
		p.Request = p.Optimize.Request
		if p.Optimize.Degraded() {
			g.degraded("optimizer", string(p.Optimize.Outcome))
		}
	} else {
		p.Optimize = optimize.Result{Request: req, Outcome: optimize.OutcomeApplied}
	}

	// The router sees a projection with no message content. That boundary is
	// what makes a Decision a complete record of why an endpoint was chosen —
	// a decision that depended on prompt text could not be replayed from one.
	// Nil under AssumeCredentials, which is what makes a keyless dry run still
	// produce a full ranking: a nil set has no opinion and eliminates nothing.
	// The dry-run response reports separately which refs the caller actually
	// holds, so the explanation stays complete without becoming misleading.
	// Prepared.Credentials always holds what the caller *actually* has, even
	// when routing was told to ignore it. The explanation needs both facts: the
	// ranking is over the whole catalog, and the reader still has to be told
	// which rows they could act on.
	p.Credentials = opts.Credentials
	p.AssumedCredentials = opts.AssumeCredentials

	routeCreds := opts.Credentials
	if opts.AssumeCredentials {
		routeCreds = nil
	}

	d, err := routing.Route(routing.Input{
		Request: p.Request.RoutingView(),
		Catalog: cat,
		Policy:  pol,
		// Endpoints with an open breaker are eliminated during filtering rather
		// than failed during execution, so the recorded ranking stays honest
		// about what was actually available at decision time.
		Health:      g.Health.Snapshot(),
		Credentials: routeCreds,
	})
	if err != nil {
		return nil, err
	}

	// The one field on a Decision that Route does not write. Recorded here
	// rather than beside the Decision because the Decision is what gets
	// disclosed, logged, and replayed, and an optimization the customer cannot
	// see is indistinguishable from a bug (ADR-0008).
	d.Optimizations = p.Optimize.Applied
	p.Decision = d

	if d.UsedFallback {
		// Filtering eliminated every candidate and the route's fallback (or the
		// baseline) was served instead. The request succeeded, which is the
		// point — but a route whose constraints exclude everything it lists is
		// misconfigured, and nothing else would ever say so.
		g.degraded("routing", "no_viable_candidate")
	}

	p.resolveCacheKey(g.Cache)
	return p, nil
}

// resolveCacheKey decides whether this request participates in the cache.
//
// Deliberately after routing: the chosen endpoint is part of the key, because
// two models given the same prompt do not give the same answer. Checking before
// routing would save a few microseconds of scoring and key entries on a model
// that might not be the one serving them.
func (p *Prepared) resolveCacheKey(store *respcache.Store) {
	switch {
	case store == nil:
		p.CacheSkip = respcache.ReasonRouteDisabled
		return
	case p.PinnedStrict:
		p.CacheSkip = respcache.ReasonPinnedStrict
		return
	}

	reason, ok := respcache.Cacheable(p.Route, p.Request, p.TenantID())
	if !ok {
		p.CacheSkip = reason
		return
	}

	scope := p.CacheScope()
	if scope == "" {
		p.CacheSkip = respcache.ReasonAnonymousBYOK
		return
	}
	p.CacheKey = respcache.Key(scope, p.Decision.Chosen, p.Request)
}

// cachePrincipalSchema versions the principal derivation below, independently
// of the key schema, so either can change without the other.
const cachePrincipalSchema = "relay/cache-principal/v1"

// CacheScope is the response cache's unit of isolation.
//
// Normally the tenant, which is what it has always been and remains correct for
// a deployment whose credentials come from its own environment: everyone sharing
// a tenant is, by construction, the same customer.
//
// Under caller-supplied credentials that stops being true, and the failure is
// severe. Two strangers evaluating a hosted Relay both authenticate as the
// anonymous default tenant and both bring their own provider keys. Same tenant,
// same endpoint, same prompt — so the same key, and a hit. One of them receives
// an answer generated on somebody else's credential, together with confirmation
// that the other person asked that exact question.
//
// The existing guard is one condition short of catching it: Cacheable already
// refuses an *empty* tenant, with a comment saying an unscoped entry is exactly
// the cross-tenant hit this package must be unable to produce — but "default" is
// not empty, so it passes. The intent was right and the check could not see the
// case.
//
// So the scope narrows to the tenant plus a principal derived from the key
// material actually presented. The principal is a truncated salted digest: never
// the key, never reversible into one, never logged, and never placed in the
// dry-run body. It exists only to make two different credential holders hash
// differently.
func (p *Prepared) CacheScope() string {
	tid := p.TenantID()
	if p.requestCreds == nil {
		return tid
	}
	principal := p.requestCreds.Principal(cachePrincipalSchema)
	if principal == "" {
		// Credentials were supplied but nothing could be derived from them.
		// Decline rather than fall back to the tenant, which is precisely the
		// boundary that does not hold here.
		return ""
	}
	return tid + "\x00" + principal
}

// credentialResolver is the chain this request resolves credentials through.
//
// Nil when the caller supplied nothing, so the executor falls back to its own —
// which keeps this change additive rather than a migration.
func (p *Prepared) credentialResolver() provider.Resolver {
	if p.requestCreds == nil {
		return nil
	}
	// Deliberately *not* including the deployment's own resolver here: the
	// executor already falls back to it when this returns nil, and building a
	// chain would require this layer to hold a reference to it. The chain that
	// matters — caller first, environment second — is assembled in cmd/relay,
	// where both halves are already in scope.
	return p.requestCreds
}

// TenantID is the identifier every ledger entry is filed under.
func (p *Prepared) TenantID() string {
	if p.Tenant == nil {
		return tenant.DefaultID
	}
	return p.Tenant.ID
}

// Breakpoints counts the cache markers the optimizer placed.
//
// Recomputed from the request rather than trusted from the Optimization record,
// because the record says how many the lever intended and this says how many
// are actually on the wire. When those two disagree the second one is the truth,
// and it is the one that has to be read against the provider's reported cache
// reads.
func (p *Prepared) Breakpoints() int {
	if p.Request == nil {
		return 0
	}
	n := 0
	for _, c := range p.Request.System {
		if c.CacheBreakpoint {
			n++
		}
	}
	for _, t := range p.Request.Tools {
		if t.CacheBreakpoint {
			n++
		}
	}
	for _, m := range p.Request.Messages {
		for _, c := range m.Parts {
			if c.CacheBreakpoint {
				n++
			}
		}
	}
	return n
}

// NewRecord builds a ledger entry from everything decided before execution.
//
// The costs are filled in afterwards by Record.Price, once the provider has
// reported actual token counts. Estimates are for admission control and routing
// only; the ledger uses actuals (architecture §8).
func (p *Prepared) NewRecord(streaming bool) meter.Record {
	d := p.Decision
	return meter.Record{
		RequestID:      p.Request.ID,
		Tenant:         p.TenantID(),
		RouteName:      d.RouteName,
		CatalogVersion: d.CatalogVersion,
		PolicyVersion:  d.PolicyVersion,
		Mode:           d.Baseline.Mode,
		RequestedModel: p.RequestedModel,
		Endpoint:       d.Chosen,
		BaselineID:     d.Baseline.EndpointID,
		BaselineSource: d.Baseline.Source,
		Streaming:      streaming,
		Substituted:    d.Substituted(),
		UsedFallback:   d.UsedFallback,
		Optimizations:  domain.LeverNames(d.Optimizations),
		Breakpoints:    p.Breakpoints(),
	}
}

// SubstitutedModel reports whether a *different model* answered.
//
// Distinct from Decision.Substituted, which compares endpoint IDs, and the
// distinction became load-bearing the moment failover existed. An endpoint is a
// (model, deployment, credential) triple, so moving from us-east to eu-west
// changes the endpoint and not the answer — reporting that as a substitution
// tells a caller they were served a cheaper model when they were served the
// same one from a different region.
//
// The narrower reading is the one the product claim is about, so it is the one
// the disclosure header and the substitutions counter use.
func (p *Prepared) SubstitutedModel() bool {
	if p.Decision == nil || !p.Decision.Substituted() {
		return false
	}
	served, ok := p.Catalog.Endpoint(p.Decision.Chosen)
	base, ok2 := p.Catalog.Endpoint(p.Decision.Baseline.EndpointID)
	if !ok || !ok2 {
		// Cannot tell. Report the substitution: over-disclosing is a worse
		// header and a better default than quietly under-disclosing one.
		return true
	}
	return served.Model != base.Model || served.Provider != base.Provider
}

// FailedOver reports that a different endpoint answered while the model stayed
// the same — a recovery rather than a substitution.
func (p *Prepared) FailedOver() bool {
	return p.Decision != nil && p.Decision.Substituted() && !p.SubstitutedModel()
}

// RecordAttempts folds the execution trail into a ledger entry.
//
// Called after execution rather than folded into NewRecord, because a record is
// built before the first provider call — it has to be, so that a failure has
// somewhere to be written down.
func (p *Prepared) RecordAttempts(r *meter.Record) {
	r.Attempts = len(p.Attempts)
	r.Retries = p.Attempts.Retries()
	r.Failovers = p.Attempts.Failovers()
	r.Rerouted = p.Rerouted
	// Chosen may have moved during execution, and the ledger records what
	// answered rather than what was selected.
	r.Endpoint = p.Decision.Chosen
	// The narrower reading: the substitutions figure in a savings report is the
	// product's claim about serving a different model, not a count of regional
	// failovers.
	r.Substituted = p.SubstitutedModel()
}

// executeRequest builds the executor call for this prepared request.
//
// The Reroute callback is the interesting field. A Reroute class means the
// constraint set used for routing was wrong — almost always a context estimate
// that four-bytes-per-token got wrong on non-English text — so the executor
// hands back a corrected request and this re-runs the same router with the same
// policy against the same catalog snapshot. Same inputs but one, which is what
// makes the second decision explainable next to the first.
func (p *Prepared) executeRequest(streaming bool) execute.Request {
	return execute.Request{
		Request:   p.Request,
		Catalog:   p.Catalog,
		Decision:  p.Decision,
		Streaming: streaming,
		Reroute:   execute.Rerouter(p.Catalog, p.Policy),
		// Caller-supplied keys first, the deployment's own second. Nil when the
		// request brought none, which leaves the executor on its own resolver —
		// exactly the behaviour every deployment had before BYOK existed.
		Resolver: p.credentialResolver(),
	}
}

// absorb folds the executor's outcome back into the decision.
//
// Mutating the Decision after routing, deliberately and for the same reason the
// optimizations are attached to it: the Decision is what gets recorded and
// disclosed, and a ledger that named the endpoint Relay *intended* to use would
// bill a customer against a model that never ran. After a failover or a reroute,
// Chosen is the endpoint that actually answered.
func (p *Prepared) absorb(res execute.Result) {
	p.Attempts = res.Attempts
	p.Rerouted = res.Rerouted
	p.Escalation = res.Escalation
	p.Discarded = res.Discarded

	if res.Endpoint != nil && res.Endpoint.ID != "" {
		p.Decision.Chosen = res.Endpoint.ID
	}
}

// ObserveTTFT reports a stream's time-to-first-token to the health tracker.
//
// Reported from the server rather than the executor because the executor is
// gone by then: it returns once the stream is open, and the first token arrives
// later. TTFT is the right latency signal for a stream — total duration is
// dominated by how long the answer is, which is a property of the request
// rather than of the endpoint, so feeding it to the scorer would rank endpoints
// by the verbosity of whoever happened to call them.
func (g *Gateway) ObserveTTFT(p *Prepared, ttft time.Duration) {
	if g.Health == nil || ttft <= 0 || p.CacheHit {
		return
	}
	ep, ok := p.Catalog.Endpoint(p.Decision.Chosen)
	if !ok {
		return
	}
	g.Health.Observe(healthpkg.Key{Endpoint: ep.ID, Credential: ep.CredentialRef}, "", ttft)
}

// Chat executes a prepared non-streaming request, serving from cache when it
// can and populating the cache when it cannot.
//
// Retries and failover happen inside the executor; what happens here is
// recording their outcome, because the ledger has to say which endpoint
// actually answered rather than which one was chosen.
func (g *Gateway) Chat(ctx context.Context, p *Prepared) (*provider.Response, *domain.ModelEndpoint, execute.Attempt, error) {
	if e, ok := g.cacheGet(p); ok {
		ep, _ := p.Catalog.Endpoint(p.Decision.Chosen)
		p.CacheHit = true
		p.CachedUsage = e.Usage
		return e.Response(), ep, execute.Attempt{EndpointID: p.Decision.Chosen}, nil
	}

	res, err := g.Executor.Run(ctx, p.executeRequest(false))
	p.absorb(res)

	if err == nil && res.Response != nil {
		g.cachePut(p, res.Response.ID, res.Response.Parts, res.Response.FinishReason, res.Response.Usage)
	}
	g.observeQuality(p, res)
	return res.Response, res.Endpoint, res.Attempts.Last(), err
}

// observeQuality feeds the escalation outcome back into the health tracker.
//
// This is the half of ADR-0009 that makes the cascade a control loop rather than
// a safety net. Escalating recovers one request; feeding the rate back is what
// stops the same endpoint being chosen for the next thousand.
//
// The endpoint credited is the one that *produced the answer that was judged* —
// on an escalation that is the downgrade whose output failed, not the baseline
// that rescued it. Attributing it to the baseline would penalise the endpoint
// that did its job.
func (g *Gateway) observeQuality(p *Prepared, res execute.Result) {
	if g.Health == nil || p.CacheHit {
		return
	}
	if esc := res.Escalation; esc != nil {
		g.Health.ObserveServed(esc.From, true)
		return
	}
	if res.Endpoint != nil {
		g.Health.ObserveServed(res.Endpoint.ID, false)
	}
}

// Stream executes a prepared streaming request. The caller owns the returned
// Stream and must close it on every path.
//
// A cache hit is returned as a Stream rather than as a special case, so the SSE
// framing, tool-call assembly, cancellation, and disconnect handling in the
// handler above run once and are exercised by both paths.
func (g *Gateway) Stream(ctx context.Context, p *Prepared) (provider.Stream, *domain.ModelEndpoint, execute.Attempt, error) {
	if e, ok := g.cacheGet(p); ok {
		ep, _ := p.Catalog.Endpoint(p.Decision.Chosen)
		p.CacheHit = true
		p.CachedUsage = e.Usage
		return e.Stream(), ep, execute.Attempt{EndpointID: p.Decision.Chosen}, nil
	}

	res, err := g.Executor.Run(ctx, p.executeRequest(true))
	p.absorb(res)

	st, ep, att := res.Stream, res.Endpoint, res.Attempts.Last()
	if err != nil || p.CacheKey == "" {
		return st, ep, att, err
	}

	// Recorded on its way past, and stored only at a clean end of stream. A
	// cancelled or failed stream is a partial answer, and a partial answer in a
	// cache is the worst kind of entry: it looks complete to everyone who reads
	// it afterwards.
	return respcache.Record(st, func(parts []domain.ContentPart, finish provider.FinishReason, u provider.Usage) {
		g.cachePut(p, "", parts, finish, u)
	}), ep, att, err
}

func (g *Gateway) cacheGet(p *Prepared) (*respcache.Entry, bool) {
	if g.Cache == nil || p.CacheKey == "" {
		return nil, false
	}
	return g.Cache.Get(p.CacheKey, p.CacheScope(), p.Decision.Chosen)
}

func (g *Gateway) cachePut(
	p *Prepared, id string, parts []domain.ContentPart,
	finish provider.FinishReason, u provider.Usage,
) {
	if g.Cache == nil || p.CacheKey == "" || len(parts) == 0 {
		return
	}
	ttl := domain.DefaultCacheTTL
	if p.Route != nil {
		ttl = p.Route.Cache.EffectiveTTL()
	}
	g.Cache.Put(p.CacheKey, &respcache.Entry{
		Scope:        p.CacheScope(),
		Endpoint:     p.Decision.Chosen,
		ProviderID:   id,
		Parts:        parts,
		FinishReason: finish,
		Usage:        u,
	}, ttl)
}

// Price fills a record's cost figures from reported usage.
//
// Every figure comes from the *same* token counts, priced two or three ways
// against the catalog snapshot this request was routed under. The input-token
// baseline is exact; the output-token baseline is the actual output count at
// baseline rates rather than a modelled guess at what a different model would
// have produced. That approximation is documented rather than hidden inside a
// model nobody can audit.
//
// On a cache hit the usage passed in is the *stored* usage — what this request
// would have consumed — and the cost of serving it was nothing. Delegating to
// meter.Record keeps the invariants between Cost, BaselineCost, Saved, and
// ShadowSaved in one place. They are easy to get subtly wrong and a savings
// report is built entirely out of them.
func (p *Prepared) Price(r *meter.Record, u provider.Usage) {
	if p.CacheHit {
		r.PriceCacheHit(u, p.Catalog, p.Decision)
		return
	}
	r.Price(u, p.Catalog, p.Decision)
	p.priceDiscarded(r)
}

// priceDiscarded folds the cost of abandoned attempts into the record.
//
// This is the arithmetic that makes cascade escalation honest. The served cost
// above is the baseline's, priced against a baseline of the same endpoint — a
// saving of exactly zero. Adding what the failed downgrade cost pushes Saved
// negative by that amount, which is the truth: the customer paid for two answers
// and used one.
//
// It runs after Price rather than inside it because Price is the definition of
// the single-attempt case and every other caller depends on that meaning.
func (p *Prepared) priceDiscarded(r *meter.Record) {
	if len(p.Discarded) == 0 {
		return
	}
	for _, c := range p.Discarded {
		ep, ok := p.Catalog.Endpoint(c.EndpointID)
		if !ok {
			continue
		}
		cost := c.Usage.Cost(ep)
		r.DiscardedCost += cost
		r.Cost += cost
	}

	if r.SavingMeasured {
		// Recomputed rather than adjusted, so there is one expression of what
		// Saved means and it stays true after this.
		r.Saved = r.BaselineCost - r.Cost
	}

	if e := p.Escalation; e != nil {
		r.Escalated = true
		r.EscalatedFrom = e.From
		r.EscalationCause = string(e.Reason)
		r.EscalationRecovered = e.Recovered
	}
}
