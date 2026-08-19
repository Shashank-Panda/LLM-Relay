package routing

import (
	"fmt"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// candidate is an endpoint that has survived filtering, with the per-request
// facts the scorer needs already computed.
type candidate struct {
	ep     *domain.ModelEndpoint
	cost   domain.Money
	health domain.EndpointHealth
}

// filterCtx carries everything the hard constraints are evaluated against.
type filterCtx struct {
	req          *domain.Request
	route        *domain.Route
	policy       *domain.Policy
	health       *domain.Health
	mode         domain.BaselineMode
	baselineCost domain.Money
	hasBaseline  bool
}

// filter applies hard constraints. Constraints eliminate; weights rank.
//
// Each candidate is eliminated by the first constraint it fails, and the checks
// run in a fixed order chosen so the reported reason is the most fundamental
// one: structural impossibility first (retired, missing capability, context too
// small), then permission, then cost and quality, then transient health. A
// candidate rejected as CircuitOpen is one that would otherwise have worked.
func filter(c filterCtx, ids []string, cat *domain.Catalog) ([]candidate, []domain.RejectedCandidate) {
	kept := make([]candidate, 0, len(ids))
	var rejected []domain.RejectedCandidate

	reject := func(id string, reason domain.RejectReason, detail string) {
		rejected = append(rejected, domain.RejectedCandidate{
			EndpointID: id, Reason: reason, Detail: detail,
		})
	}

	for _, id := range ids {
		ep, ok := cat.Endpoint(id)
		if !ok {
			reject(id, domain.RejectUnknownEndpoint, "not present in catalog "+cat.Version)
			continue
		}

		if ep.Lifecycle.Status == domain.StatusRetired {
			detail := "endpoint is retired"
			if ep.Lifecycle.Replacement != "" {
				detail += "; replaced by " + ep.Lifecycle.Replacement
			}
			reject(id, domain.RejectDeprecated, detail)
			continue
		}

		if missing, ok := ep.Supports(c.req.Need); !ok {
			reject(id, domain.RejectMissingCapability, "request requires "+missing)
			continue
		}

		if !ep.Fits(c.req.Estimate) {
			reject(id, domain.RejectContextTooSmall, fmt.Sprintf(
				"needs %d tokens (%d in + %d max out), window is %d",
				c.req.Estimate.InputTokens+c.req.Estimate.MaxOutputTokens,
				c.req.Estimate.InputTokens, c.req.Estimate.MaxOutputTokens,
				ep.Limits.ContextWindow))
			continue
		}

		// Read once and reused: the health snapshot supplies the circuit state,
		// the latency estimate the scorer will need, and the quality penalty
		// that several checks below apply.
		h := c.health.For(id)

		// Route-level requirements are the route author's own hard constraints,
		// checked before tenant policy so a misconfigured route reports as a
		// route problem rather than as a policy denial.
		if detail, ok := satisfiesRequirements(ep, h, c.route.Require); !ok {
			reject(id, requirementReason(detail), detail.msg)
			continue
		}

		if detail, ok := c.policy.Permits(ep); !ok {
			reject(id, domain.RejectPolicyDenied, detail)
			continue
		}

		if ep.CredentialRef == "" {
			reject(id, domain.RejectNoCredential, "no credential configured for this endpoint")
			continue
		}

		cost := ep.EstimatedCost(c.req.Estimate)

		// The baseline is a ceiling: Relay may serve something cheaper than the
		// caller asked for, never something dearer. It cannot upsell.
		if c.mode == domain.ModeOptimize && c.hasBaseline && cost > c.baselineCost {
			reject(id, domain.RejectAboveBaseline, fmt.Sprintf(
				"estimated %s exceeds baseline %s", cost, c.baselineCost))
			continue
		}

		// The quality floor is a filter and not a weight. Scores are normalized
		// across the surviving set, so a weight expresses a preference relative
		// to whatever else happens to be available — it cannot express a floor.
		// See ADR-0009.
		if dim, floor, ok := belowFloor(ep, h, c.route, c.policy); !ok {
			// The effective score is reported, not the asserted one, with the
			// penalty spelled out beside it. An operator reading "quality.coding
			// is 0.85, floor is 0.80" next to a rejection would conclude the
			// filter was broken; the number that actually decided is the one
			// observation produced.
			reject(id, domain.RejectBelowQualityFloor, fmt.Sprintf(
				"quality.%s is %.2f (asserted %.2f, observed penalty %.2f), floor is %.2f",
				dim, h.EffectiveQuality(ep.QualityFor(dim)), ep.QualityFor(dim),
				h.QualityPenalty, floor))
			continue
		}

		if c.policy != nil && c.policy.MaxCostPerRequest > 0 && cost > c.policy.MaxCostPerRequest {
			reject(id, domain.RejectBudgetExceeded, fmt.Sprintf(
				"estimated %s exceeds per-request cap %s", cost, c.policy.MaxCostPerRequest))
			continue
		}

		if h.CircuitOpen {
			reject(id, domain.RejectCircuitOpen, "circuit breaker is open")
			continue
		}

		kept = append(kept, candidate{ep: ep, cost: cost, health: h})
	}

	return kept, rejected
}

type requirementFailure struct {
	capability bool
	msg        string
}

func requirementReason(f requirementFailure) domain.RejectReason {
	if f.capability {
		return domain.RejectMissingCapability
	}
	return domain.RejectBelowQualityFloor
}

func satisfiesRequirements(
	ep *domain.ModelEndpoint, h domain.EndpointHealth, reqs []domain.Constraint,
) (requirementFailure, bool) {
	for _, r := range reqs {
		if r.Capability != "" && !hasCapability(ep, r.Capability) {
			return requirementFailure{
				capability: true,
				msg:        "route requires " + r.Capability,
			}, false
		}
		// The route's own quality requirement is checked against the effective
		// score for the same reason the tenant floor is: an asserted score that
		// observation has contradicted is not the number to decide on.
		if r.QualityDim != "" {
			q := h.EffectiveQuality(ep.QualityFor(r.QualityDim))
			if q < r.QualityMin {
				return requirementFailure{
					msg: fmt.Sprintf("route requires quality.%s >= %.2f, endpoint is %.2f",
						r.QualityDim, r.QualityMin, q),
				}, false
			}
		}
	}
	return requirementFailure{}, true
}

func hasCapability(ep *domain.ModelEndpoint, name string) bool {
	switch name {
	case "tools":
		return ep.Capabilities.Tools
	case "vision":
		return ep.Capabilities.Vision
	case "json_schema":
		return ep.Capabilities.JSONSchema
	case "reasoning":
		return ep.Capabilities.Reasoning
	case "streaming":
		return ep.Capabilities.Streaming
	}
	// An unrecognised capability name is treated as unsatisfiable rather than
	// ignored: a typo in a constraint must not silently disable it.
	return false
}

// belowFloor checks the tenant's quality floor against the dimensions this
// route actually ranks on. A floor for a dimension the route never scores is
// not applied — a coding floor should not eliminate candidates on a
// summarization route, where the score means something else entirely.
func belowFloor(
	ep *domain.ModelEndpoint, h domain.EndpointHealth, rt *domain.Route, pol *domain.Policy,
) (dim string, floor float64, ok bool) {
	if pol == nil || len(pol.QualityFloor) == 0 {
		return "", 0, true
	}
	for _, k := range domain.SortedKeys(rt.Weights) {
		d, isQuality := strings.CutPrefix(k, domain.DimQualityPrefix)
		if !isQuality {
			continue
		}
		f := pol.Floor(d)
		// Measured against the effective score, which is what closes ADR-0009's
		// loop. The floor exists because catalog scores are operator assertions
		// and assertions can be optimistic; applying it to the unadjusted
		// assertion would leave the floor trusting precisely the number that
		// observation has shown to be wrong.
		if f > 0 && h.EffectiveQuality(ep.QualityFor(d)) < f {
			return d, f, false
		}
	}
	return "", 0, true
}
