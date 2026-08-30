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
	"sort"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

var (
	// ErrNoRequest and ErrNoCatalog are programming errors, not routing outcomes.
	ErrNoRequest = errors.New("routing: request is required")
	ErrNoCatalog = errors.New("routing: catalog is required")

	// ErrNoCandidate means filtering eliminated everything and there was no
	// fallback and no baseline to fall back to. The caller cannot serve this
	// request at all.
	//
	// Prefer errors.Is over comparison: the router returns a *NoCandidateError
	// carrying the rejections, and that type reports as this sentinel.
	ErrNoCandidate = errors.New("routing: no viable candidate and no fallback")
)

// NoCandidateError is ErrNoCandidate with the reasons attached.
//
// The bare sentinel was returned as a flat 503 saying only that nothing could
// serve the request, which is unactionable — and under BYOK the overwhelmingly
// common cause is "you sent no key", which is a caller problem fixable in one
// header rather than a server problem to wait out. The rejections are already
// computed; the only thing missing was carrying them far enough to be reported.
type NoCandidateError struct {
	Rejected []domain.RejectedCandidate
}

func (e *NoCandidateError) Error() string {
	if len(e.Rejected) == 0 {
		return ErrNoCandidate.Error()
	}
	reasons := make([]string, 0, len(e.Rejected))
	seen := map[domain.RejectReason]bool{}
	for _, r := range e.Rejected {
		if !seen[r.Reason] {
			seen[r.Reason] = true
			reasons = append(reasons, string(r.Reason))
		}
	}
	sort.Strings(reasons)
	return ErrNoCandidate.Error() + " (" + strings.Join(reasons, ", ") + ")"
}

// Is makes errors.Is(err, ErrNoCandidate) keep working, so adding this type
// changed no existing call site.
func (e *NoCandidateError) Is(target error) bool { return target == ErrNoCandidate }

// AllRejectedFor reports whether every rejection has this reason, and there was
// at least one.
//
// "Every" rather than "any" on purpose: a request where one endpoint lacked a
// credential and the rest failed a quality floor is not a credentials problem,
// and telling the caller to send a key would send them after the wrong thing.
func (e *NoCandidateError) AllRejectedFor(reason domain.RejectReason) bool {
	if len(e.Rejected) == 0 {
		return false
	}
	for _, r := range e.Rejected {
		if r.Reason != reason {
			return false
		}
	}
	return true
}

// MissingRefs lists the credential refs named by NoCredential rejections.
func (e *NoCandidateError) MissingRefs(cat *domain.Catalog) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range e.Rejected {
		if r.Reason != domain.RejectNoCredential || cat == nil {
			continue
		}
		ep, ok := cat.Endpoint(r.EndpointID)
		if !ok || ep.CredentialRef == "" || seen[ep.CredentialRef] {
			continue
		}
		seen[ep.CredentialRef] = true
		out = append(out, ep.CredentialRef)
	}
	sort.Strings(out)
	return out
}

// Input is everything Route is allowed to look at.
type Input struct {
	Request *domain.Request
	Catalog *domain.Catalog
	Policy  *domain.Policy
	Health  *domain.Health

	// Credentials is which credential refs the caller can actually use, taken
	// as a snapshot outside so this function stays pure. Nil has no opinion and
	// keeps every candidate.
	Credentials *domain.CredentialSet
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
			return nil, &NoCandidateError{}
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
		d.Failover = equivalentEndpoints(cat, rt, baseline, in.Health)
		return d, nil
	}

	fc := filterCtx{
		req: req, route: rt, policy: pol, health: in.Health,
		creds: in.Credentials,
		mode:  mode, baselineCost: baselineCost, hasBaseline: hasBaseline,
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
	//
	// Note what is deliberately *not* called here. serveBaseline replaces Ranked
	// with a single synthetic entry, which is right for strict mode and for a
	// fallback — no scoring happened, so reporting a ranking would imply a
	// comparison that was never made. In shadow mode the comparison is the
	// entire product: the ranking is the evidence for the counterfactual, and
	// discarding it would leave a savings report nobody could drill into.
	if mode == domain.ModeShadow && hasBaseline {
		d.Counterfactual = best.EndpointID
		d.CounterfactualCost = best.Cost
		setServed(d, baseline.ID, baselineCost)
		// Shadow serves the baseline, so its failover rules are strict mode's.
		// Falling over into the ranking would serve the counterfactual for real,
		// which is precisely the substitution shadow mode exists to *not* make.
		d.Failover = equivalentEndpoints(cat, rt, baseline, in.Health)
		return d, nil
	}

	setServed(d, best.EndpointID, best.Cost)
	// Optimize mode already has the tenant's permission to choose among these,
	// so the ranking is the failover order.
	for _, c := range d.Ranked[1:] {
		d.Failover = append(d.Failover, c.EndpointID)
	}
	return d, nil
}

// equivalentEndpoints lists endpoints that serve the same model as the one
// chosen, for a request that may not be substituted.
//
// This is what makes reliability and strictness compatible rather than opposed.
// A strict tenant said "do not answer me with a different model" — they did not
// say "if us-east is down, fail my request". An endpoint is a (model,
// deployment, credential) triple, so the same model in another region is a
// different endpoint and the same answer, and switching between them honours
// the promise exactly.
//
// Matching on Model rather than on endpoint ID is the whole mechanism: nothing
// here can select a cheaper or different model, no matter how the route is
// configured, because a different model has a different name.
func equivalentEndpoints(
	cat *domain.Catalog, rt *domain.Route, chosen *domain.ModelEndpoint, h *domain.Health,
) []string {
	if chosen == nil {
		return nil
	}

	// The route's candidates plus every catalog entry, because an operator who
	// deployed a second region for redundancy has not necessarily listed it as
	// a routing candidate — and for a pinned model there may be no route at all.
	seen := map[string]bool{chosen.ID: true}
	var out []string

	consider := func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true

		ep, ok := cat.Endpoint(id)
		switch {
		case !ok:
		case ep.Model != chosen.Model:
			// A different model. Not available here at any price.
		case ep.Provider != chosen.Provider:
			// Same model name at a different vendor is a different model.
		case ep.Lifecycle.Status == domain.StatusRetired:
		case ep.CredentialRef == "":
		case h.For(id).CircuitOpen:
			// Already known bad. Listing it would spend an attempt discovering
			// what the breaker already established.
		default:
			out = append(out, id)
		}
	}

	if rt != nil {
		for _, id := range rt.Candidates {
			consider(id)
		}
	}
	for _, id := range domain.SortedKeys(cat.Endpoints) {
		consider(id)
	}
	return out
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

	return nil, &NoCandidateError{Rejected: d.Rejected}
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
