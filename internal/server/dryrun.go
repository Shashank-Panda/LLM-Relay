package server

import (
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/optimize"
)

// dryRunResponse is what X-Relay-Dry-Run returns.
//
// The full explanation, not the summary the headers carry. Two audiences, and
// they want the same document: a customer asking "why did you pick that, and
// what did you change about my request", and an operator asking the same thing
// about a route they just edited. Answering both without a provider call is what
// makes a routing or lever change safe to test against production config.
//
// Every field here is read off the Decision rather than recomputed. The Decision
// *is* the explanation — a second, prettier rendering built from separate
// reasoning would be free to drift from what actually happens on a live request,
// and a dry-run that can disagree with reality is worse than none.
type dryRunResponse struct {
	Object string `json:"object"`

	RequestedModel string `json:"requested_model"`
	Tenant         string `json:"tenant"`
	Route          string `json:"route,omitempty"`
	Mode           string `json:"mode"`
	CatalogVersion string `json:"catalog_version"`
	PolicyVersion  string `json:"policy_version,omitempty"`

	Chosen      string `json:"chosen"`
	Baseline    string `json:"baseline,omitempty"`
	Substituted bool   `json:"substituted"`
	Fallback    bool   `json:"used_fallback,omitempty"`

	// Counterfactual is what optimize mode would have chosen. Present only in
	// shadow mode, where Chosen is the baseline and this is the road not taken.
	Counterfactual string `json:"counterfactual,omitempty"`

	Ranked   []dryRunCandidate `json:"ranked,omitempty"`
	Rejected []dryRunRejection `json:"rejected,omitempty"`

	Optimizations []dryRunOptimization `json:"optimizations"`
	Optimizer     dryRunOptimizer      `json:"optimizer"`

	Cache dryRunCache `json:"cache"`

	Estimate dryRunEstimate `json:"estimate"`

	Note string `json:"note"`
}

type dryRunCandidate struct {
	Endpoint string  `json:"endpoint"`
	Total    float64 `json:"total"`
	// Components is each dimension's weighted contribution, so a total can be
	// audited without re-running the scorer. This is the field that answers the
	// most common routing complaint — "why didn't it pick the better model" —
	// with arithmetic instead of an assurance.
	Components map[string]float64 `json:"components,omitempty"`
	CostUSD    float64            `json:"estimated_cost_usd"`
	Reasons    []string           `json:"reasons,omitempty"`
}

type dryRunRejection struct {
	Endpoint string `json:"endpoint"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
}

type dryRunOptimization struct {
	Lever  string `json:"lever"`
	Before string `json:"before"`
	After  string `json:"after"`
	Reason string `json:"reason"`
}

type dryRunOptimizer struct {
	// Outcome distinguishes "applied nothing because nothing is enabled" from
	// "failed open". Both produce an empty optimization list and they are not
	// the same situation: the first is a tenant's configuration and the second
	// is a defect.
	Outcome   string `json:"outcome"`
	ElapsedUS int64  `json:"elapsed_us"`
	BudgetUS  int64  `json:"budget_us"`
	Degraded  bool   `json:"degraded,omitempty"`
}

type dryRunCache struct {
	Eligible bool   `json:"eligible"`
	Key      string `json:"key,omitempty"`
	Skipped  string `json:"skipped_because,omitempty"`
}

type dryRunEstimate struct {
	// InputTokensBefore and InputTokensAfter bracket what the optimizer did to
	// the request size. Equal unless a lever changed the message list.
	InputTokensBefore int `json:"input_tokens_before"`
	InputTokensAfter  int `json:"input_tokens_after"`

	MaxOutputTokens      int `json:"max_output_tokens"`
	ExpectedOutputTokens int `json:"expected_output_tokens"`

	CostUSD         float64 `json:"estimated_cost_usd"`
	BaselineCostUSD float64 `json:"estimated_baseline_cost_usd"`
	SavedUSD        float64 `json:"estimated_saved_usd"`
	SavingMeasured  bool    `json:"saving_measured"`

	Breakpoints int `json:"cache_breakpoints"`
}

// optimizeBudgetUS is the optimizer's budget, echoed so a reader can compare it
// against the elapsed figure without knowing the constant.
const optimizeBudgetUS = int64(optimize.DefaultBudget / time.Microsecond)

const dryRunNote = "Estimates only. No provider was called and nothing was recorded. " +
	"Output length cannot be known before generation, so every figure here is priced " +
	"against an expected output count; the ledger prices the same request against " +
	"reported actuals."

func dryRun(p *gateway.Prepared) dryRunResponse {
	d := p.Decision

	out := dryRunResponse{
		Object:         "relay.dry_run",
		RequestedModel: p.RequestedModel,
		Tenant:         p.TenantID(),
		Route:          d.RouteName,
		Mode:           string(d.Baseline.Mode),
		CatalogVersion: d.CatalogVersion,
		PolicyVersion:  d.PolicyVersion,
		Chosen:         d.Chosen,
		Baseline:       d.Baseline.EndpointID,
		Substituted:    d.Substituted(),
		Fallback:       d.UsedFallback,
		Counterfactual: d.Counterfactual,
		// Never nil: an empty JSON array says "nothing was applied", where a
		// null says "this build does not report optimizations", and a client
		// distinguishing those two would be reading a bug.
		Optimizations: make([]dryRunOptimization, 0, len(d.Optimizations)),
		Optimizer: dryRunOptimizer{
			Outcome:   string(p.Optimize.Outcome),
			ElapsedUS: p.Optimize.Elapsed.Microseconds(),
			BudgetUS:  optimizeBudgetUS,
			Degraded:  p.Optimize.Degraded(),
		},
		Cache: dryRunCache{
			Eligible: p.CacheKey != "",
			Key:      p.CacheKey,
			Skipped:  string(p.CacheSkip),
		},
		Note: dryRunNote,
	}

	for _, c := range d.Ranked {
		out.Ranked = append(out.Ranked, dryRunCandidate{
			Endpoint:   c.EndpointID,
			Total:      round3(c.Total),
			Components: c.Components,
			CostUSD:    c.Cost.Dollars(),
			Reasons:    c.Reasons,
		})
	}
	for _, c := range d.Rejected {
		out.Rejected = append(out.Rejected, dryRunRejection{
			Endpoint: c.EndpointID,
			Reason:   string(c.Reason),
			Detail:   c.Detail,
		})
	}
	for _, o := range d.Optimizations {
		out.Optimizations = append(out.Optimizations, dryRunOptimization{
			Lever: o.Lever, Before: o.Before, After: o.After, Reason: o.Reason,
		})
	}

	out.Estimate = dryRunEstimate{
		InputTokensBefore:    tokensOf(p.Original),
		InputTokensAfter:     tokensOf(p.Request),
		MaxOutputTokens:      p.Request.Estimate.MaxOutputTokens,
		ExpectedOutputTokens: p.Request.Estimate.ExpectedOutputTokens,
		CostUSD:              d.EstimatedCost.Dollars(),
		BaselineCostUSD:      d.BaselineCost.Dollars(),
		SavedUSD:             d.EstimatedSaved.Dollars(),
		SavingMeasured:       d.SavingMeasured,
		Breakpoints:          p.Breakpoints(),
	}
	return out
}

func tokensOf(r *domain.NormalizedRequest) int {
	if r == nil {
		return 0
	}
	return r.InputTokens()
}
