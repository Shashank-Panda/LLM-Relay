package gateway

import (
	"context"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/routing"
)

// Gateway wires the pipeline stages together.
type Gateway struct {
	Store    *catalog.Store
	Executor *execute.Executor

	// Policy is the tenant policy. Phase 1 has one, for everybody; Phase 7
	// makes it per-tenant, which changes how this field is populated and
	// nothing about how it is used.
	Policy *domain.Policy
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
}

// Prepare resolves the baseline and routes the request.
//
// No I/O and no provider contact: everything here is a pure function over a
// catalog snapshot. The snapshot is taken once, at the top, so a request that
// begins under catalog version N completes under version N even if a reload
// lands mid-flight.
func (g *Gateway) Prepare(req *domain.NormalizedRequest, model, pin string) (*Prepared, error) {
	cat := g.Store.Current()
	if cat == nil {
		return nil, &ErrUnknownModel{Model: model}
	}

	pol := g.Policy
	if pol == nil {
		pol = domain.DefaultPolicy()
	}

	mode := ResolveMode(pin, pol)

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
	}, nil
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

// Cost prices reported usage against the served endpoint and the baseline.
//
// Both figures come from the *same* token counts, priced two ways. That is the
// whole measurement: the input-token baseline is exact, and the output-token
// baseline is the actual output count at baseline rates rather than a modelled
// guess at what a different model would have produced. Documenting that
// approximation is more honest than hiding it inside a model nobody can audit.
func (p *Prepared) Cost(u provider.Usage, served *domain.ModelEndpoint) (cost, baseline domain.Money, measured bool) {
	cost = u.Cost(served)

	if !p.Decision.SavingMeasured {
		return cost, 0, false
	}
	base, ok := p.Catalog.Endpoint(p.Decision.Baseline.EndpointID)
	if !ok {
		return cost, 0, false
	}
	return cost, u.Cost(base), true
}
