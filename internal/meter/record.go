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
	r.FromUsage(u)

	if served, ok := cat.Endpoint(d.Chosen); ok {
		r.Cost = u.Cost(served)
	}

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

	if d.Counterfactual == "" {
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
