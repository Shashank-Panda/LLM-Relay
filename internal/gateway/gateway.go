package gateway

import (
	"context"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
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
func (g *Gateway) Prepare(req *domain.NormalizedRequest, tn *tenant.Tenant, model, pin string) (*Prepared, error) {
	cat := g.Store.Current()
	if cat == nil {
		return nil, &ErrUnknownModel{Model: model}
	}

	pol := g.policyFor(tn)
	mode := ResolveMode(pin, pol)
	pinned := domain.BaselineMode(pin) == domain.ModeStrict

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
	}
	if rt, ok := cat.Route(routeName); ok {
		p.Route = rt
	}

	if !pinned {
		p.Optimize = optimize.New(pol.Levers).Run(req, g.Stats)
		p.Request = p.Optimize.Request
	} else {
		p.Optimize = optimize.Result{Request: req, Outcome: optimize.OutcomeApplied}
	}

	// The router sees a projection with no message content. That boundary is
	// what makes a Decision a complete record of why an endpoint was chosen —
	// a decision that depended on prompt text could not be replayed from one.
	d, err := routing.Route(routing.Input{
		Request: p.Request.RoutingView(),
		Catalog: cat,
		Policy:  pol,
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
	p.CacheKey = respcache.Key(p.TenantID(), p.Decision.Chosen, p.Request)
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

// Chat executes a prepared non-streaming request, serving from cache when it
// can and populating the cache when it cannot.
func (g *Gateway) Chat(ctx context.Context, p *Prepared) (*provider.Response, *domain.ModelEndpoint, execute.Attempt, error) {
	ep, _ := p.Catalog.Endpoint(p.Decision.Chosen)

	if e, ok := g.cacheGet(p); ok {
		p.CacheHit = true
		p.CachedUsage = e.Usage
		return e.Response(), ep, execute.Attempt{EndpointID: p.Decision.Chosen}, nil
	}

	resp, ep, att, err := g.Executor.Chat(ctx, p.Request, p.Catalog, p.Decision)
	if err == nil {
		g.cachePut(p, resp.ID, resp.Parts, resp.FinishReason, resp.Usage)
	}
	return resp, ep, att, err
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

	st, ep, att, err := g.Executor.Stream(ctx, p.Request, p.Catalog, p.Decision)
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
	return g.Cache.Get(p.CacheKey, p.TenantID(), p.Decision.Chosen)
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
		Tenant:       p.TenantID(),
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
}
