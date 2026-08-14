package domain

// BaselineMode controls how much freedom the router has relative to what the
// caller actually asked for. See ADR-0007.
type BaselineMode string

const (
	// ModeStrict serves exactly what was requested: no optimization, no
	// substitution. This is the zero value on purpose. Substitution is
	// something a tenant switches on, never something they arrive at.
	ModeStrict BaselineMode = "strict"

	// ModeShadow serves the baseline but computes the counterfactual anyway,
	// so a tenant can measure savings before accepting any behaviour change.
	ModeShadow BaselineMode = "shadow"

	// ModeOptimize routes freely below the baseline, subject to the quality
	// floor.
	ModeOptimize BaselineMode = "optimize"
)

func (m BaselineMode) valid() bool {
	switch m {
	case ModeStrict, ModeShadow, ModeOptimize:
		return true
	}
	return false
}

// Baseline is what the caller would have got without Relay, and how much
// latitude they granted. Resolved before routing so the counterfactual is a
// request-time fact rather than an analytics reconstruction.
type Baseline struct {
	EndpointID string
	Mode       BaselineMode
	// Source records how the baseline was determined, for the savings ledger:
	// explicit_model | route_default | tenant_default.
	Source string
}

type Estimate struct {
	InputTokens int
	// MaxOutputTokens is the ceiling, used for the context-window fit check.
	MaxOutputTokens int
	// ExpectedOutputTokens is the expectation, used for cost estimation.
	// These differ, and conflating them either overflows contexts or
	// overstates costs.
	ExpectedOutputTokens int
}

// Need is what the request requires of an endpoint, derived during
// normalization from the request body.
type Need struct {
	Tools      bool
	Vision     bool
	JSONSchema bool
	Reasoning  bool
	Streaming  bool
}

// Request is the routing-relevant projection of a normalized request.
//
// It deliberately carries no message content. The router must not be able to
// make a decision it cannot explain from structured fields — if content
// mattered to a decision, that decision could not appear in a Decision.
type Request struct {
	ID     string
	Tenant string

	// RouteName is resolved upstream. When a caller pins a real model, the
	// normalizer sets Baseline.EndpointID to that model and RouteName to the
	// tenant's default route; the router does not re-derive either.
	RouteName string

	Baseline Baseline
	Need     Need
	Estimate Estimate

	SessionKey string
	// PreviousEndpoint served the last turn of this session, if any. Drives
	// prompt-cache affinity — switching endpoints discards a warm prefix cache
	// and often costs more than the substitution saves.
	PreviousEndpoint string
}
