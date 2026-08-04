// Package routing decides which model endpoint serves a request.
//
// Route is a pure function: it performs no I/O, consults no clock, and uses no
// randomness. Everything it needs arrives as an argument — a catalog snapshot,
// a policy, a health snapshot — and everything it concludes leaves in the
// returned Decision. That shape is the reason the router can be exhaustively
// table-tested, the reason a past decision can be replayed exactly from its
// recorded catalog and policy versions, and the reason the explainability API
// needs no separate code path: the Decision *is* the explanation.
//
// Execution — retries, failover, cascade escalation, streaming — lives in the
// executor. Routing never knows about HTTP.
package routing

import (
	"errors"

	"github.com/Shashank-Panda/relay/internal/domain"
)

var (
	// ErrNoRequest and ErrNoCatalog are programming errors, not routing outcomes.
	ErrNoRequest = errors.New("routing: request is required")
	ErrNoCatalog = errors.New("routing: catalog is required")

	// ErrNoCandidate means filtering eliminated everything and there was no
	// fallback and no baseline to fall back to. The caller cannot serve this
	// request at all.
	ErrNoCandidate = errors.New("routing: no viable candidate and no fallback")
)

// Input is everything Route is allowed to look at.
type Input struct {
	Request *domain.Request
	Catalog *domain.Catalog
	Policy  *domain.Policy
	Health  *domain.Health
}

// Route selects an endpoint and explains why.
func Route(in Input) (*domain.Decision, error) {
	req, cat := in.Request, in.Catalog
	if req == nil {
		return nil, ErrNoRequest
	}
	if cat == nil {
		return nil, ErrNoCatalog
	}
	pol := in.Policy
	if pol == nil {
		pol = domain.DefaultPolicy()
	}

	baseline, hasBaseline := cat.Endpoint(req.Baseline.EndpointID)
	baselineCost := domain.Money(0)
	if hasBaseline {
		baselineCost = baseline.EstimatedCost(req.Estimate)
	}

	d := &domain.Decision{
		RouteName:      req.RouteName,
		CatalogVersion: cat.Version,
		PolicyVersion:  pol.Version,
		Baseline:       req.Baseline,
		BaselineCost:   baselineCost,
		SavingMeasured: hasBaseline,
	}

	rt, ok := cat.Route(req.RouteName)
	if !ok {
		// An unknown route is an operator error, but it must not be the
		// caller's outage. Degrade to the baseline where one exists; only fail
		// when there is genuinely nothing to serve. See ADR-0010.
		if !hasBaseline {
			return nil, ErrNoCandidate
		}
		d.UsedFallback = true
		serveBaseline(d, baseline, baselineCost, "route not found; served baseline")
		return d, nil
	}

	mode := pol.Mode(req.Baseline.Mode)

	// Strict mode does no scoring at all. The caller asked for something
	// specific and did not grant permission to reconsider it.
	if mode == domain.ModeStrict && hasBaseline {
		serveBaseline(d, baseline, baselineCost, "strict mode: served as requested")
		return d, nil
	}

	fc := filterCtx{
		req: req, route: rt, policy: pol, health: in.Health,
		mode: mode, baselineCost: baselineCost, hasBaseline: hasBaseline,
	}
	kept, rejected := filter(fc, candidateIDs(rt, req.Baseline.EndpointID), cat)
	d.Rejected = rejected

	if len(kept) == 0 {
		return serveFallback(d, cat, rt, baseline, baselineCost, req)
	}

	d.Ranked = score(kept, req, rt)
	best := d.Ranked[0]

	// Shadow mode routes the counterfactual but serves the baseline: the tenant
	// measures the saving without yet accepting any behaviour change.
	if mode == domain.ModeShadow && hasBaseline {
		d.Counterfactual = best.EndpointID
		serveBaseline(d, baseline, baselineCost, "shadow mode: counterfactual recorded, baseline served")
		return d, nil
	}

	setServed(d, best.EndpointID, best.Cost)
	return d, nil
}

// setServed records the endpoint that will execute and the saving against the
// baseline.
//
// The saving stays zero when there is no baseline. Subtracting a cost from a
// zero baseline would report every unmeasurable request as a loss, which would
// then aggregate into the savings ledger as if it were a measured fact.
// SavingMeasured is what distinguishes "no saving" from "no baseline".
func setServed(d *domain.Decision, id string, cost domain.Money) {
	d.Chosen = id
	d.EstimatedCost = cost
	if d.SavingMeasured {
		d.EstimatedSaved = d.BaselineCost - cost
	}
}

// candidateIDs returns the route's candidates with the baseline appended if it
// is not already among them.
//
// The baseline is always a candidate so that "serve what was asked for" is
// never an option the router has quietly removed from itself.
func candidateIDs(rt *domain.Route, baselineID string) []string {
	if baselineID == "" {
		return rt.Candidates
	}
	for _, id := range rt.Candidates {
		if id == baselineID {
			return rt.Candidates
		}
	}
	ids := make([]string, 0, len(rt.Candidates)+1)
	ids = append(ids, rt.Candidates...)
	return append(ids, baselineID)
}

// serveFallback handles the case where every candidate was eliminated.
//
// The route's declared fallback wins, then the baseline. Only when neither
// exists does the request fail — "we could not satisfy the constraints" should
// resolve to a defined, boring endpoint rather than an error.
func serveFallback(
	d *domain.Decision, cat *domain.Catalog, rt *domain.Route,
	baseline *domain.ModelEndpoint, baselineCost domain.Money, req *domain.Request,
) (*domain.Decision, error) {
	d.UsedFallback = true

	if fb, ok := cat.Endpoint(rt.Fallback); ok {
		cost := fb.EstimatedCost(req.Estimate)
		setServed(d, fb.ID, cost)
		d.Ranked = []domain.ScoredCandidate{{
			EndpointID: fb.ID,
			Cost:       cost,
			Reasons:    []string{"route fallback: every candidate was eliminated"},
		}}
		return d, nil
	}

	if baseline != nil {
		serveBaseline(d, baseline, baselineCost, "no viable candidate; served baseline")
		return d, nil
	}

	return nil, ErrNoCandidate
}

// serveBaseline records a decision to serve exactly what the caller asked for.
// Total is left at zero because no scoring happened — reporting a score here
// would imply a comparison that was never made.
func serveBaseline(d *domain.Decision, ep *domain.ModelEndpoint, cost domain.Money, reason string) {
	setServed(d, ep.ID, cost)
	d.Ranked = []domain.ScoredCandidate{{
		EndpointID: ep.ID,
		Cost:       cost,
		Reasons:    []string{reason},
	}}
}
