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
// Apply is deterministic in its levers: same request, same config, same stats,
// same adjustments. It performs no I/O. It reads a clock for exactly one
// purpose — enforcing its own latency budget — and that is why the budget is
// injectable rather than implicit. It is separate from the router because it
// answers a different question: the optimizer transforms the request, the router
// selects the endpoint.
package optimize

import (
	"fmt"
	"math"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// assumedMaxOutput is the output ceiling used for the context-fit check when
// the caller set none and no ceiling could be computed.
//
// Some number is required: the filter compares input+output against each
// endpoint's context window, and treating an unset ceiling as zero would let
// requests route to endpoints they cannot actually fit in.
const assumedMaxOutput = 4096

// StatsSource answers the one question the levers ask about history.
//
// An interface rather than the concrete Stats because the production
// implementation is a live histogram maintained off the metering pipeline
// (meter.RouteStats), and building a map snapshot per request purely to satisfy
// a struct field would allocate on the hot path for nothing. Implementations
// must be safe for concurrent reads and must not block: a lever that waits on a
// lock has spent its budget on bookkeeping.
type StatsSource interface {
	// OutputP95 returns the 95th percentile of output lengths observed on this
	// route, and false when there is no sound basis for one. False must mean
	// "no basis", never "zero" — a lever that treats absent history as a small
	// number would cap every route it has never seen.
	OutputP95(route string) (int, bool)
}

// Stats is a fixed snapshot of observed per-route behaviour, for tests and for
// callers that already hold the numbers.
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

// outputP95 reads a StatsSource that may be absent.
//
// A nil *Stats is safe to call; a nil interface is not, and the two are
// different values that arrive from different places — nil *Stats from a test,
// a nil interface from a gateway with no stats configured. Checking here means
// no lever has to.
func outputP95(s StatsSource, route string) (int, bool) {
	if s == nil {
		return 0, false
	}
	return s.OutputP95(route)
}

// DefaultBudget is the latency the optimizer is allowed to spend.
//
// Three milliseconds, from ADR-0008. It is a p99 target for the whole
// component against a 5 ms p50 gateway overhead SLO, which means the budget is
// not decoration: a lever that overruns it is spending the customer's latency
// to save the customer's money, and past this point that trade stops being
// worth making.
const DefaultBudget = 3 * time.Millisecond

// Outcome is how an optimization pass ended. Recorded rather than inferred,
// because passthrough is silent by design — without this a gateway can be
// optimizing nothing at all and still look perfectly healthy.
type Outcome string

const (
	// OutcomeApplied means every enabled lever ran to completion.
	OutcomeApplied Outcome = "applied"

	// OutcomeOverBudget means the budget was exhausted partway through. The
	// levers that had already run are kept — each one is independently valid,
	// and discarding correct work to reach a tidier state would cost the
	// customer money for nothing.
	OutcomeOverBudget Outcome = "over_budget"

	// OutcomePanicked means a lever crashed and the original request was served
	// untouched. Optimization is optional and inference is not (ADR-0010).
	OutcomePanicked Outcome = "panicked"
)

// Result is one optimization pass.
type Result struct {
	// Request is the request to execute. Never nil when the input was non-nil:
	// on every failure path it is the original.
	Request *domain.NormalizedRequest

	Applied []domain.Optimization
	Outcome Outcome
	Elapsed time.Duration
}

// Degraded reports whether this pass failed open. Distinct from "applied
// nothing", which is the correct and silent outcome of a tenant with no levers
// enabled.
func (r Result) Degraded() bool {
	return r.Outcome == OutcomeOverBudget || r.Outcome == OutcomePanicked
}

type Optimizer struct {
	cfg domain.LeverConfig

	// budget bounds the whole pass. Zero disables the check, which is what the
	// determinism tests want: with no clock consulted, Apply is a pure function
	// of its arguments.
	budget time.Duration

	// now is injected so the budget can be tested without sleeping. A test that
	// proves a 3 ms budget by taking 3 ms is a test nobody runs on every commit.
	now func() time.Time
}

func New(cfg domain.LeverConfig) *Optimizer {
	return &Optimizer{cfg: cfg, budget: DefaultBudget, now: time.Now}
}

// WithBudget overrides the latency budget. Zero disables the check entirely.
func (o *Optimizer) WithBudget(d time.Duration) *Optimizer {
	c := *o
	c.budget = d
	return &c
}

// WithClock replaces the clock, for tests that need to drive the budget.
func (o *Optimizer) WithClock(now func() time.Time) *Optimizer {
	c := *o
	c.now = now
	return &c
}

// Apply returns an optimized copy of req and the list of adjustments made.
//
// Retained as the two-value form because it is what reads well in tests and at
// call sites that do not care how the pass ended. Anything that reports on the
// optimizer — metrics, degradation alerts, dry-run — wants Run instead.
func (o *Optimizer) Apply(req *domain.NormalizedRequest, stats StatsSource) (*domain.NormalizedRequest, []domain.Optimization) {
	r := o.Run(req, stats)
	return r.Request, r.Applied
}

// Run applies every enabled lever within the budget and reports how it went.
//
// The input is never mutated. The original must survive for retries, failover,
// and the baseline cost comparison — and an optimizer that edited in place
// would make "what did the caller actually send" unanswerable after the fact.
//
// Two fail-open paths, for the same reason: optimization is optional and
// inference is not, so a defect here must degrade to an unoptimized request
// rather than to a failed one (ADR-0010).
//
//   - A panic in any lever abandons the whole pass and serves the original.
//     Nothing partially applied escapes, because a request half-rewritten by a
//     crashing lever is not a request anybody reasoned about.
//   - Exhausting the budget stops the remaining levers and keeps what already
//     ran. Each lever is independently valid and independently recorded, so
//     there is nothing to unwind — and throwing away correct work to reach a
//     tidier state would cost the customer money to no end.
func (o *Optimizer) Run(req *domain.NormalizedRequest, stats StatsSource) (res Result) {
	if req == nil {
		return Result{Outcome: OutcomeApplied}
	}

	start := o.clock()

	defer func() {
		if r := recover(); r != nil {
			// Deliberately swallowed rather than re-raised. There is no useful
			// place to report it from here — the optimizer has no logger by
			// design, because a component inside a 3 ms budget should not be
			// formatting strings — and the OutcomePanicked counter is what makes
			// the failure visible.
			res = Result{
				Request: req,
				Outcome: OutcomePanicked,
				Elapsed: o.since(start),
			}
		}
	}()

	out := req.Clone()
	res = Result{Request: out, Outcome: OutcomeApplied}

	// Order matters. Pruning changes the message list, so breakpoints must be
	// placed against the final content or they would mark parts that are no
	// longer there.
	levers := []func() *domain.Optimization{
		func() *domain.Optimization { return o.pruneContext(out) },
		func() *domain.Optimization { return o.placeCacheBreakpoints(out) },
		func() *domain.Optimization { return o.applyDefaultEffort(out) },
		func() *domain.Optimization { return o.applyOutputCeiling(out, stats) },
	}

	for _, lever := range levers {
		if o.exhausted(start) {
			res.Outcome = OutcomeOverBudget
			break
		}
		if op := lever(); op != nil {
			res.Applied = append(res.Applied, *op)
		}
	}

	// Always recomputed, including on the over-budget path. The estimate feeds
	// the router's context-window filter and its cost model; leaving it stale
	// after a lever has changed the request would route against a request that
	// no longer exists, which is a correctness bug rather than a lost saving.
	o.recomputeEstimate(out, stats)

	res.Elapsed = o.since(start)
	return res
}

// clock reads the injected clock, or nothing at all when the budget is off.
//
// Skipping the read is what keeps Apply a pure function under a zero budget:
// there is no point consulting a clock whose answer cannot change the outcome,
// and two calls to time.Now are two syscall-ish reads on the hot path.
func (o *Optimizer) clock() time.Time {
	if o.budget <= 0 || o.now == nil {
		return time.Time{}
	}
	return o.now()
}

func (o *Optimizer) since(start time.Time) time.Duration {
	if start.IsZero() || o.now == nil {
		return 0
	}
	return o.now().Sub(start)
}

// exhausted reports whether the budget is spent.
//
// Checked between levers rather than inside them. A lever is a bounded walk
// over a request that is already in memory, so the granularity is fine; the
// alternative is threading a deadline through every helper to interrupt work
// that was going to finish in microseconds anyway.
func (o *Optimizer) exhausted(start time.Time) bool {
	if o.budget <= 0 || start.IsZero() {
		return false
	}
	return o.now().Sub(start) >= o.budget
}

// recomputeEstimate refreshes the token estimate after the levers have run.
//
// Easy to forget and expensive to omit: pruning changes the input size and the
// output ceiling changes the worst case, and both feed the router's
// context-window filter and its cost estimate. Stale numbers here would route
// against a request that no longer exists.
func (o *Optimizer) recomputeEstimate(req *domain.NormalizedRequest, stats StatsSource) {
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
	if p95, ok := outputP95(stats, req.RouteName); ok && p95 < expected {
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
func (o *Optimizer) applyOutputCeiling(req *domain.NormalizedRequest, stats StatsSource) *domain.Optimization {
	if !o.cfg.OutputCeiling {
		return nil
	}
	if req.Params.MaxTokens != nil {
		return nil // the caller decided
	}
	p95, ok := outputP95(stats, req.RouteName)
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
