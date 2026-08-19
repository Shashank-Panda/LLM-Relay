// Package meter records what every request cost, and what it would have cost.
//
// This is the savings ledger. It is the product's evidence, so its correctness
// bar is higher than the rest of the data plane: a routing bug produces a bad
// answer the customer can see, and a metering bug produces a number on an
// invoice-adjacent report that nobody can check.
//
// Two rules follow from that, and both are enforced by the types rather than by
// discipline:
//
//   - Unmeasured is never zero. A request with no baseline carries
//     SavingMeasured=false, and no aggregate may add it to a measured zero.
//   - A shadow saving is never a real saving. Money that could have been saved
//     lives in its own field from here to the report.
//
// The pipeline never blocks a request. Records go to a buffered channel drained
// by one worker; when the buffer is full they are dropped and counted, because
// a gateway that stalls inference to write telemetry has inverted its own
// priorities.
package meter

import (
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// Outcome is how a request ended.
type Outcome string

const (
	OutcomeSuccess   Outcome = "success"
	OutcomeError     Outcome = "error"
	OutcomeCancelled Outcome = "cancelled"
)

// Record is one request's ledger entry.
//
// Carries token counts, costs, and identifiers — never prompt or response
// content. That is a product commitment (architecture §1), and the way to keep
// it is to have no field that could hold one.
type Record struct {
	RequestID string    `json:"request_id"`
	Tenant    string    `json:"tenant"`
	At        time.Time `json:"at"`

	RouteName      string `json:"route,omitempty"`
	CatalogVersion string `json:"catalog_version,omitempty"`
	PolicyVersion  string `json:"policy_version,omitempty"`

	// Mode is the baseline mode this request ran under. The savings report is
	// grouped by it, because a shadow month and an optimize month are different
	// claims.
	Mode domain.BaselineMode `json:"mode"`

	// RequestedModel is what the caller put in the model field; Endpoint is what
	// actually served. They differ exactly when Substituted is true.
	RequestedModel string `json:"requested_model"`
	Endpoint       string `json:"endpoint"`
	BaselineID     string `json:"baseline_endpoint,omitempty"`
	BaselineSource string `json:"baseline_source,omitempty"`

	Streaming    bool `json:"streaming"`
	Substituted  bool `json:"substituted"`
	UsedFallback bool `json:"used_fallback,omitempty"`

	// Optimizations names the levers applied to this request. Names only; the
	// before/after detail goes to the caller in headers and to the operator in
	// the completion log. The ledger needs enough to answer "which levers are
	// earning their keep", and a ledger line is not a place to put prose.
	Optimizations []string `json:"optimizations,omitempty"`

	// Breakpoints is how many cache markers were inserted.
	//
	// Recorded next to CachedInputTokens on purpose. A breakpoint in the wrong
	// place is silently useless — the request succeeds, the cost is unchanged,
	// and nothing anywhere reports a problem — so the inserted count is only
	// meaningful when read against what the provider actually said it cached.
	// See ADR-0008.
	Breakpoints int `json:"breakpoints,omitempty"`

	// CacheHit means this answer came from the exact-match response cache and
	// no provider was called. Cost is therefore genuinely zero, which is the
	// one place in this struct where a zero cost is a fact rather than a gap.
	CacheHit bool `json:"cache_hit,omitempty"`

	// FinishReason is why generation stopped. Carried for one specific reason:
	// `length` on a request whose ceiling Relay set is the optimizer truncating
	// somebody's answer, and a lever that does that regularly is misconfigured
	// rather than working (ADR-0008).
	FinishReason string `json:"finish_reason,omitempty"`

	// Attempts is how many provider calls this request made, Retries how many
	// repeated an endpoint, and Failovers how many endpoints were abandoned.
	//
	// All three are cost figures before they are reliability figures. Every
	// attempt past the first is a second charge for one answer, so a rising
	// retry rate shows up on an invoice before it shows up on an error
	// dashboard — and the ledger is where those two facts can be read together.
	Attempts  int `json:"attempts,omitempty"`
	Retries   int `json:"retries,omitempty"`
	Failovers int `json:"failovers,omitempty"`

	// Rerouted records that a provider rejected the request on a constraint the
	// router had wrong, and the router was re-run with it corrected. A rising
	// rate means the token estimate is systematically off for some traffic
	// shape, which is a fixable modelling problem rather than a provider one.
	Rerouted bool `json:"rerouted,omitempty"`

	// Escalated marks a request where a downgraded endpoint produced invalid
	// output and the baseline was retried (ADR-0009).
	//
	// The fields beside it are what make the escalation honest rather than
	// merely recorded. DiscardedCost is the money spent on the answer that was
	// thrown away, and it is included in Cost — so an escalated request has a
	// *negative* Saved, which is the correct arithmetic and the uncomfortable
	// one. A savings ledger that excluded its own failures would be measuring
	// the wrong thing.
	Escalated       bool         `json:"escalated,omitempty"`
	EscalatedFrom   string       `json:"escalated_from,omitempty"`
	EscalationCause string       `json:"escalation_cause,omitempty"`
	DiscardedCost   domain.Money `json:"discarded_cost_micros,omitempty"`

	// EscalationRecovered is whether the baseline's answer passed the same check
	// the downgrade failed. False means a second call was bought and the output
	// is still invalid, which says something about the request rather than about
	// the endpoint.
	EscalationRecovered bool `json:"escalation_recovered,omitempty"`

	// StreamFailedAfterTTFT marks a stream that broke after the client had
	// already received content.
	//
	// Its own field rather than an error class, because it is the one failure
	// ADR-0003 knowingly does not cover: past the first byte no failover is
	// honest, so this is the residual risk of that decision. The ADR says it
	// gets revisited with data if the gap turns out to be larger than expected,
	// and this is the data.
	StreamFailedAfterTTFT bool `json:"stream_failed_after_ttft,omitempty"`

	InputTokens       int  `json:"input_tokens"`
	CachedInputTokens int  `json:"cached_input_tokens,omitempty"`
	OutputTokens      int  `json:"output_tokens"`
	ReasoningTokens   int  `json:"reasoning_tokens,omitempty"`
	UsageEstimated    bool `json:"usage_estimated,omitempty"`

	// Cost is the served endpoint priced against reported actuals.
	Cost domain.Money `json:"cost_micros"`

	// BaselineCost prices the *same* token counts against the baseline
	// endpoint. Input tokens are exact; the output count is the actual one at
	// baseline rates rather than a guess at what a different model would have
	// produced. That approximation is documented rather than modelled.
	BaselineCost domain.Money `json:"baseline_cost_micros,omitempty"`

	// Saved is BaselineCost − Cost, and is meaningful only when
	// SavingMeasured is true.
	Saved          domain.Money `json:"saved_micros,omitempty"`
	SavingMeasured bool         `json:"saving_measured"`

	// Counterfactual is what optimize mode would have chosen, priced against
	// the same actuals. Populated only in shadow mode.
	Counterfactual     string       `json:"counterfactual,omitempty"`
	CounterfactualCost domain.Money `json:"counterfactual_cost_micros,omitempty"`
	ShadowSaved        domain.Money `json:"shadow_saved_micros,omitempty"`

	Outcome    Outcome `json:"outcome"`
	ErrorClass string  `json:"error_class,omitempty"`

	// Duration is the whole request; ProviderDuration is the part spent waiting
	// on the vendor. The difference is Relay's own overhead, and separating
	// them is what makes "are we slow or is the provider slow" answerable.
	//
	// The _ns suffix is not decoration. time.Duration marshals as an int64 of
	// nanoseconds, so a field named duration_ms carrying a Duration would be
	// wrong by a factor of a million — and wrong in a way that looks entirely
	// plausible to anyone reading the ledger.
	Duration         time.Duration `json:"duration_ns"`
	ProviderDuration time.Duration `json:"provider_duration_ns"`
	TTFT             time.Duration `json:"ttft_ns,omitempty"`
}

// Overhead is the time Relay itself added.
//
// Clamped at zero. The two timers start at different points and a clock
// adjustment can make the subtraction negative; a negative overhead in a
// histogram is worse than a zero one.
func (r Record) Overhead() time.Duration {
	d := r.Duration - r.ProviderDuration
	if d < 0 {
		return 0
	}
	return d
}

// FromUsage fills the token fields from a provider's reported usage.
func (r *Record) FromUsage(u provider.Usage) {
	r.InputTokens = u.InputTokens
	r.CachedInputTokens = u.CachedInputTokens
	r.OutputTokens = u.OutputTokens
	r.ReasoningTokens = u.ReasoningTokens
	r.UsageEstimated = u.Estimated
}

// Price computes every cost figure from reported usage and a catalog snapshot.
//
// One function rather than three call sites, because the invariants between
// these fields are the ones a savings report depends on and they are easy to
// get subtly wrong: Saved must stay zero when unmeasured, and the shadow figure
// must never leak into it.
func (r *Record) Price(u provider.Usage, cat *domain.Catalog, d *domain.Decision) {
	r.price(u, cat, d, false)
}

// PriceCacheHit prices a request answered from the response cache.
//
// Separate from Price rather than a boolean on it, because the arithmetic is
// genuinely different and the difference is the entire claim: no provider was
// called, so Cost is zero, and the saving is the *whole* baseline cost rather
// than a difference between two prices. The usage passed in is the stored
// usage from the original call — it is what the request would have consumed,
// which is exactly what makes the counterfactual computable.
//
// The counterfactual is deliberately not computed here. In shadow mode the
// shadow figure answers "what would substitution have saved", and nothing was
// executed to substitute; reporting a substitution saving on a request that
// made no call would be inventing one.
func (r *Record) PriceCacheHit(u provider.Usage, cat *domain.Catalog, d *domain.Decision) {
	r.price(u, cat, d, true)
}

func (r *Record) price(u provider.Usage, cat *domain.Catalog, d *domain.Decision, cacheHit bool) {
	r.FromUsage(u)
	r.CacheHit = cacheHit
	r.Optimizations = domain.LeverNames(d.Optimizations)

	if !cacheHit {
		if served, ok := cat.Endpoint(d.Chosen); ok {
			r.Cost = u.Cost(served)
		}
	}
	// Cost stays zero on a cache hit. No tokens were bought, so the honest
	// figure is zero — and it is what makes a response cache show up as a real
	// measured saving in strict mode rather than as an unexplained gap.

	r.SavingMeasured = d.SavingMeasured
	if !d.SavingMeasured {
		// Unmeasured. Both figures stay zero so that a report summing them
		// cannot silently dilute a real measurement with an absent one.
		return
	}

	base, ok := cat.Endpoint(d.Baseline.EndpointID)
	if !ok {
		r.SavingMeasured = false
		return
	}
	r.BaselineCost = u.Cost(base)
	r.Saved = r.BaselineCost - r.Cost

	if d.Counterfactual == "" || cacheHit {
		return
	}
	// Shadow mode: the baseline was served, so Saved above is correctly zero.
	// This is the separate number — what optimize mode would have saved on
	// exactly these tokens.
	if cf, ok := cat.Endpoint(d.Counterfactual); ok {
		r.Counterfactual = d.Counterfactual
		r.CounterfactualCost = u.Cost(cf)
		r.ShadowSaved = r.BaselineCost - r.CounterfactualCost
	}
}
