package gateway

import (
	"context"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/provider"
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
}

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

	// RequestedModel is what the caller put in the model field. Echoed back
	// verbatim, because clients key caches and dashboards on it.
	RequestedModel string

	// Tenant owns this request and its costs.
	Tenant *tenant.Tenant
}

// Prepare resolves the baseline and routes the request.
//
// No I/O and no provider contact: everything here is a pure function over a
// catalog snapshot. The snapshot is taken once, at the top, so a request that
// begins under catalog version N completes under version N even if a reload
// lands mid-flight.
func (g *Gateway) Prepare(req *domain.NormalizedRequest, tn *tenant.Tenant, model, pin string) (*Prepared, error) {
	cat := g.Store.Current()
	if cat == nil {
		return nil, &ErrUnknownModel{Model: model}
	}

	pol := g.policyFor(tn)
	mode := ResolveMode(pin, pol)

	if tn != nil {
		req.Tenant = tn.ID
	}

	routeName, baseline, err := ResolveBaseline(cat, model, mode)
	if err != nil {
		return nil, err
	}

	req.RouteName = routeName
	req.Baseline = baseline

	// The router sees a projection with no message content. That boundary is
	// what makes a Decision a complete record of why an endpoint was chosen —
	// a decision that depended on prompt text could not be replayed from one.
	d, err := routing.Route(routing.Input{
		Request: req.RoutingView(),
		Catalog: cat,
		Policy:  pol,
	})
	if err != nil {
		return nil, err
	}

	return &Prepared{
		Request:        req,
		Catalog:        cat,
		Decision:       d,
		RequestedModel: model,
		Tenant:         tn,
	}, nil
}

// TenantID is the identifier every ledger entry is filed under.
func (p *Prepared) TenantID() string {
	if p.Tenant == nil {
		return tenant.DefaultID
	}
	return p.Tenant.ID
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
	}
}

// Chat executes a prepared non-streaming request.
func (g *Gateway) Chat(ctx context.Context, p *Prepared) (*provider.Response, *domain.ModelEndpoint, execute.Attempt, error) {
	return g.Executor.Chat(ctx, p.Request, p.Catalog, p.Decision)
}

// Stream executes a prepared streaming request. The caller owns the returned
// Stream and must close it on every path.
func (g *Gateway) Stream(ctx context.Context, p *Prepared) (provider.Stream, *domain.ModelEndpoint, execute.Attempt, error) {
	return g.Executor.Stream(ctx, p.Request, p.Catalog, p.Decision)
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
// Delegating to meter.Record keeps the invariants between Cost, BaselineCost,
// Saved, and ShadowSaved in one place. They are easy to get subtly wrong and a
// savings report is built entirely out of them.
func (p *Prepared) Price(r *meter.Record, u provider.Usage) {
	r.Price(u, p.Catalog, p.Decision)
}
