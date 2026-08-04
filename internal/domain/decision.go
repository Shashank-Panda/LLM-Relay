package domain

// RejectReason names the constraint that eliminated a candidate.
//
// Recording these is as important as recording the ranking: the most common
// routing question in production is "why didn't it pick X", and this is the
// only way to answer it without reproducing the request.
type RejectReason string

const (
	RejectUnknownEndpoint   RejectReason = "UnknownEndpoint"
	RejectDeprecated        RejectReason = "Deprecated"
	RejectMissingCapability RejectReason = "MissingCapability"
	RejectContextTooSmall   RejectReason = "ContextTooSmall"
	RejectPolicyDenied      RejectReason = "PolicyDenied"
	RejectAboveBaseline     RejectReason = "AboveBaseline"
	RejectBelowQualityFloor RejectReason = "BelowQualityFloor"
	RejectNoCredential      RejectReason = "NoCredential"
	RejectCircuitOpen       RejectReason = "CircuitOpen"
	RejectBudgetExceeded    RejectReason = "BudgetExceeded"
)

type ScoredCandidate struct {
	EndpointID string
	Total      float64
	// Components holds each dimension's weighted contribution, so a total can
	// be audited without re-running the scorer.
	Components map[string]float64
	Cost       Money
	Reasons    []string
}

type RejectedCandidate struct {
	EndpointID string
	Reason     RejectReason
	Detail     string
}

// Decision is the router's entire output and the explainability payload.
// The Decision *is* the explanation — there is no second, prettier rendering
// of the reasoning that could drift from what actually happened.
type Decision struct {
	RouteName      string
	CatalogVersion string
	PolicyVersion  string

	Baseline Baseline

	// Chosen is the endpoint that will be executed.
	Chosen string

	// Counterfactual is what optimize mode would have chosen. Set only in
	// shadow mode, where Chosen is the baseline and this is the road not taken.
	Counterfactual string

	Ranked   []ScoredCandidate
	Rejected []RejectedCandidate

	// BaselineCost prices the same request against the baseline endpoint;
	// EstimatedCost prices it against Chosen. Both are estimates — the ledger
	// recomputes them from reported actuals once the response completes.
	BaselineCost   Money
	EstimatedCost  Money
	EstimatedSaved Money

	// SavingMeasured is false when no baseline existed to compare against.
	// Unmeasured is not zero, and aggregating the two together would silently
	// dilute every savings report that contains one.
	SavingMeasured bool

	// UsedFallback records that filtering eliminated everything and the route's
	// fallback (or the baseline) was served instead.
	UsedFallback bool
}

// Substituted reports whether the served endpoint differs from what the caller
// asked for. When true, disclosure headers are mandatory, not optional.
func (d *Decision) Substituted() bool {
	return d.Baseline.EndpointID != "" && d.Chosen != d.Baseline.EndpointID
}
