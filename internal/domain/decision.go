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

	// CounterfactualCost prices the counterfactual endpoint. Set only in shadow
	// mode, and it is the number the whole mode exists to produce.
	//
	// It is not EstimatedSaved. In shadow mode the baseline *is* served, so the
	// actual saving is genuinely zero — reporting the counterfactual saving
	// there would claim money that was never saved. The shadow figure is
	// BaselineCost − CounterfactualCost, kept in its own field so the two can
	// never be summed by accident.
	CounterfactualCost Money

	Ranked   []ScoredCandidate
	Rejected []RejectedCandidate

	// Failover is the ordered list of endpoints the executor may try if Chosen
	// fails, and it is deliberately not simply "the rest of Ranked".
	//
	// Failing over changes which model answers, which is the one thing a strict
	// tenant refused. So the permitted set depends on the mode, and computing it
	// here rather than in the executor keeps that judgement in the pure,
	// replayable component: a decision's failover options are part of why it was
	// made, and the executor should be reading a plan rather than inventing one
	// during an incident.
	//
	// Empty means the request either succeeds on Chosen or fails, which is the
	// correct answer for a pinned model with no equivalent endpoint behind it.
	Failover []string

	// Optimizations lists every adjustment the optimizer made to the request,
	// with before and after values.
	//
	// It lives on the Decision rather than beside it because the Decision is
	// what gets disclosed, recorded, and replayed — and an optimization the
	// customer cannot see is indistinguishable from a bug (ADR-0008). The
	// router does not produce these; the pipeline fills them in between
	// optimization and execution, which is the one field on this struct that
	// Route itself never writes.
	Optimizations []Optimization

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

// ShadowSaving is what optimize mode would have saved on this request.
//
// Reported separately from EstimatedSaved and never added to it: one is money
// that was saved, the other is money that could have been. A ledger that summed
// them would report a customer's shadow month as though they had already banked
// it, which is the single most damaging error this product could make.
func (d *Decision) ShadowSaving() (Money, bool) {
	if d.Counterfactual == "" || !d.SavingMeasured {
		return 0, false
	}
	return d.BaselineCost - d.CounterfactualCost, true
}
