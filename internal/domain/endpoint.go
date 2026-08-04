package domain

// ProviderID names an adapter, not a routing target. See ADR-0001.
type ProviderID string

type LifecycleStatus string

const (
	StatusGA         LifecycleStatus = "ga"
	StatusPreview    LifecycleStatus = "preview"
	StatusDeprecated LifecycleStatus = "deprecated"
	StatusRetired    LifecycleStatus = "retired"
)

// Capabilities is what an endpoint can do.
//
// Every field is opt-in: the zero value is a text-only, non-streaming endpoint.
// That is deliberate — a catalog entry someone forgot to finish should fail the
// capability filter and be excluded, not silently claim it can do everything.
type Capabilities struct {
	Streaming  bool
	Tools      bool
	JSONSchema bool
	Vision     bool
	Reasoning  bool
}

type Limits struct {
	ContextWindow   int
	MaxOutputTokens int
}

type Pricing struct {
	Input       Rate
	CachedInput Rate
	Output      Rate
	Reasoning   Rate
}

type Lifecycle struct {
	Status LifecycleStatus
	// Replacement is the endpoint ID that supersedes this one. Reported in the
	// rejection detail so "why did this stop working" answers itself.
	Replacement string
}

// ModelEndpoint is the routing unit: a specific, callable, priced model in a
// specific deployment. Routing to a *provider* is not a decision — Opus and
// Haiku differ by an order of magnitude in cost. See ADR-0001.
type ModelEndpoint struct {
	ID            string
	Provider      ProviderID
	Model         string
	Deployment    string
	CredentialRef string

	Capabilities Capabilities
	Limits       Limits
	Pricing      Pricing

	// Quality maps a dimension ("coding", "reasoning") to a 0..1 score.
	// These are operator assertions, not measurements. Treating them as
	// measurements is the mistake ADR-0009 exists to prevent.
	Quality map[string]float64

	Lifecycle Lifecycle
}

// EstimatedCost prices a request against this endpoint.
//
// Input tokens are known exactly; output tokens are not, so this is an
// estimate and is used for routing only. Budgets and the savings ledger use
// reported actuals (architecture §8).
func (e *ModelEndpoint) EstimatedCost(est Estimate) Money {
	return e.Pricing.Input.Cost(est.InputTokens) +
		e.Pricing.Output.Cost(est.ExpectedOutputTokens)
}

// QualityFor returns the asserted score for a dimension, or 0 if the catalog
// makes no claim. Zero is the right default: an unscored endpoint fails any
// non-zero quality floor rather than passing it by omission.
func (e *ModelEndpoint) QualityFor(dim string) float64 {
	if e.Quality == nil {
		return 0
	}
	return e.Quality[dim]
}

// Fits reports whether the request's worst-case token usage fits the context
// window. Uses MaxOutputTokens, not the expectation: a request that fits on
// average and fails at the tail is a request that fails.
func (e *ModelEndpoint) Fits(est Estimate) bool {
	if e.Limits.ContextWindow <= 0 {
		return false
	}
	return est.InputTokens+est.MaxOutputTokens <= e.Limits.ContextWindow
}

// Supports reports whether the endpoint can serve everything the request needs.
func (e *ModelEndpoint) Supports(n Need) (missing string, ok bool) {
	switch {
	case n.Tools && !e.Capabilities.Tools:
		return "tools", false
	case n.Vision && !e.Capabilities.Vision:
		return "vision", false
	case n.JSONSchema && !e.Capabilities.JSONSchema:
		return "json_schema", false
	case n.Reasoning && !e.Capabilities.Reasoning:
		return "reasoning", false
	case n.Streaming && !e.Capabilities.Streaming:
		return "streaming", false
	}
	return "", true
}
