// Package optimize rewrites a request to cost less without changing what the
// model is asked.
//
// This is the half of cost reduction that carries no quality risk. Choosing a
// cheaper endpoint trades quality for money and needs a floor, an escalation
// path, and the customer's consent. Placing a cache breakpoint or capping an
// unbounded output ceiling trades nothing — the same model answers the same
// question — which is why these levers work in strict mode, why a cautious
// customer accepts them first, and why they ship before routing does.
//
// Apply is pure and deterministic: same request, same config, same stats, same
// output. It performs no I/O and reads no clock. It is separate from the router
// because it answers a different question — the optimizer transforms the
// request, the router selects the endpoint.
package optimize

import (
	"fmt"
	"math"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// assumedMaxOutput is the output ceiling used for the context-fit check when
// the caller set none and no ceiling could be computed.
//
// Some number is required: the filter compares input+output against each
// endpoint's context window, and treating an unset ceiling as zero would let
// requests route to endpoints they cannot actually fit in.
const assumedMaxOutput = 4096

// Stats is a snapshot of observed per-route behaviour.
//
// A snapshot, not a query. The optimizer must not perform I/O, for the same
// reason the router must not: it sits on the hot path inside a 3 ms budget, and
// a component that reads a database cannot be table-tested.
type Stats struct {
	// OutputTokensP95 maps a route name to the 95th percentile of the output
	// lengths it has actually produced.
	OutputTokensP95 map[string]int
}

// OutputP95 is nil-safe: absent history means no basis for a ceiling, and no
// basis means the lever declines to act rather than inventing a number.
func (s *Stats) OutputP95(route string) (int, bool) {
	if s == nil || s.OutputTokensP95 == nil {
		return 0, false
	}
	v, ok := s.OutputTokensP95[route]
	if !ok || v <= 0 {
		return 0, false
	}
	return v, true
}

type Optimizer struct {
	cfg domain.LeverConfig
}

func New(cfg domain.LeverConfig) *Optimizer { return &Optimizer{cfg: cfg} }

// Apply returns an optimized copy of req and the list of adjustments made.
//
// The input is never mutated. The original must survive for retries, failover,
// and the baseline cost comparison — and an optimizer that edited in place
// would make "what did the caller actually send" unanswerable after the fact.
//
// It recovers from panics and returns the request untouched. That is not
// defensive habit: optimization is optional and inference is not, so a bug in
// this package must degrade to an unoptimized request rather than to a failed
// one. See ADR-0010.
func (o *Optimizer) Apply(req *domain.NormalizedRequest, stats *Stats) (out *domain.NormalizedRequest, applied []domain.Optimization) {
	if req == nil {
		return nil, nil
	}

	defer func() {
		if r := recover(); r != nil {
			out, applied = req, nil
		}
	}()

	out = req.Clone()

	// Order matters. Pruning changes the message list, so breakpoints must be
	// placed against the final content or they would mark parts that are no
	// longer there.
	add := func(op *domain.Optimization) {
		if op != nil {
			applied = append(applied, *op)
		}
	}
	add(o.pruneContext(out))
	add(o.placeCacheBreakpoints(out))
	add(o.applyDefaultEffort(out))
	add(o.applyOutputCeiling(out, stats))

	o.recomputeEstimate(out, stats)
	return out, applied
}

// recomputeEstimate refreshes the token estimate after the levers have run.
//
// Easy to forget and expensive to omit: pruning changes the input size and the
// output ceiling changes the worst case, and both feed the router's
// context-window filter and its cost estimate. Stale numbers here would route
// against a request that no longer exists.
func (o *Optimizer) recomputeEstimate(req *domain.NormalizedRequest, stats *Stats) {
	req.Estimate.InputTokens = req.InputTokens()

	maxOut := assumedMaxOutput
	if req.Params.MaxTokens != nil && *req.Params.MaxTokens > 0 {
		maxOut = *req.Params.MaxTokens
	} else if o.cfg.MaxOutputCeiling > 0 {
		maxOut = o.cfg.MaxOutputCeiling
	}
	req.Estimate.MaxOutputTokens = maxOut

	// Cost is estimated against what output is actually expected, not against
	// the ceiling. Using the ceiling would price every request as its worst
	// case and systematically over-estimate the cost of cheap endpoints.
	expected := maxOut
	if p95, ok := stats.OutputP95(req.RouteName); ok && p95 < expected {
		expected = p95
	}
	req.Estimate.ExpectedOutputTokens = expected
}

// applyDefaultEffort fills in a reasoning effort the caller did not set.
//
// It fills in; it never lowers. Reducing an effort the caller explicitly chose
// would contradict a stated decision, which is a different act from supplying a
// default for a field they left blank. A tenant who wants a hard cap on effort
// should express that as policy, not as an optimization.
func (o *Optimizer) applyDefaultEffort(req *domain.NormalizedRequest) *domain.Optimization {
	if !o.cfg.EffortDownshift || !o.cfg.DefaultEffort.Valid() {
		return nil
	}
	if req.Params.ReasoningEffort != nil {
		return nil // the caller decided
	}

	effort := o.cfg.DefaultEffort
	req.Params.ReasoningEffort = &effort
	req.Params.EffortFromOptimizer = true

	return &domain.Optimization{
		Lever:  domain.LeverEffort,
		Before: "unset",
		After:  string(effort),
		Reason: "caller set no reasoning effort; applied the tenant default instead of the provider's",
	}
}

// applyOutputCeiling bounds an unbounded max_tokens.
//
// Worth being precise about what this saves, because the obvious reading is
// wrong: providers bill generated tokens, not the ceiling, so capping
// max_tokens does not make a short answer cheaper. It pays in two other ways.
//
// It bounds the worst case, which is what turns an unbounded spend into a
// predictable one. And — the larger effect — it widens the candidate set. A
// nominal max_tokens of 64000 makes every request fail the context-window check
// on smaller, cheaper endpoints, so an untouched ceiling quietly forces routing
// onto expensive large-context models for requests that produce 300 tokens.
func (o *Optimizer) applyOutputCeiling(req *domain.NormalizedRequest, stats *Stats) *domain.Optimization {
	if !o.cfg.OutputCeiling {
		return nil
	}
	if req.Params.MaxTokens != nil {
		return nil // the caller decided
	}
	p95, ok := stats.OutputP95(req.RouteName)
	if !ok {
		return nil // no history is no basis
	}

	slack := o.cfg.OutputCeilingSlack
	if slack < 1 {
		// Below 1 the ceiling would truncate the majority of responses. Treat a
		// misconfiguration as "no slack" rather than acting on it.
		slack = 1
	}
	ceiling := int(math.Ceil(float64(p95) * slack))
	ceiling = clamp(ceiling, o.cfg.MinOutputCeiling, o.cfg.MaxOutputCeiling)
	if ceiling <= 0 {
		return nil
	}

	req.Params.MaxTokens = &ceiling
	return &domain.Optimization{
		Lever:  domain.LeverMaxTokens,
		Before: "unset",
		After:  fmt.Sprintf("%d", ceiling),
		Reason: fmt.Sprintf("route p95 output is %d tokens; ceiling set at %.2gx", p95, slack),
	}
}

func clamp(v, min, max int) int {
	if min > 0 && v < min {
		v = min
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}
