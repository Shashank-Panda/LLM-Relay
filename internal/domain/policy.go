package domain

import "strings"

// Policy is tenant-scoped configuration layered over a route.
//
// Policy may reject a candidate. It may not substitute one: a denial fails the
// request rather than quietly answering from something else. Optimization mode
// is not an exception to that — it is permission the tenant granted in advance,
// bounded below the baseline, and disclosed on every affected response.
type Policy struct {
	Version string

	// Allow, when non-empty, restricts routing to matching endpoints.
	// Deny always wins over Allow.
	Allow []string
	Deny  []string

	// Regions, when non-empty, restricts routing to endpoints whose
	// Deployment matches. Data residency is a filter, never a preference.
	Regions []string

	// MaxCostPerRequest, when > 0, eliminates candidates whose estimated cost
	// exceeds it.
	MaxCostPerRequest Money

	// OptimizationMode is the tenant default. The zero value is ModeStrict,
	// which is the point: nobody gets substituted by forgetting to configure
	// something.
	OptimizationMode BaselineMode

	// QualityFloor maps a dimension to a hard minimum. Candidates below it are
	// eliminated, never merely down-ranked — scores are relative to the
	// surviving set, so a weight cannot express a floor. See ADR-0009.
	QualityFloor map[string]float64
}

// DefaultPolicy is the safe policy: strict mode, no substitution, no limits.
func DefaultPolicy() *Policy {
	return &Policy{Version: "default", OptimizationMode: ModeStrict}
}

// Permits reports whether an endpoint passes the policy's allow/deny and
// residency rules.
func (p *Policy) Permits(e *ModelEndpoint) (detail string, ok bool) {
	if p == nil {
		return "", true
	}
	for _, pat := range p.Deny {
		if matchPattern(pat, e.ID) {
			return "denied by pattern " + pat, false
		}
	}
	if len(p.Allow) > 0 {
		allowed := false
		for _, pat := range p.Allow {
			if matchPattern(pat, e.ID) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "not in allow list", false
		}
	}
	if len(p.Regions) > 0 {
		for _, r := range p.Regions {
			if r == e.Deployment {
				return "", true
			}
		}
		return "deployment " + e.Deployment + " outside permitted regions", false
	}
	return "", true
}

// Floor returns the quality minimum for a dimension, or 0 if unconstrained.
func (p *Policy) Floor(dim string) float64 {
	if p == nil || p.QualityFloor == nil {
		return 0
	}
	return p.QualityFloor[dim]
}

// Mode resolves the effective baseline mode for a request. An explicit mode on
// the request wins — that is how X-Relay-Pin: strict escapes optimization.
func (p *Policy) Mode(reqMode BaselineMode) BaselineMode {
	if reqMode.valid() {
		return reqMode
	}
	if p != nil && p.OptimizationMode.valid() {
		return p.OptimizationMode
	}
	return ModeStrict
}

// matchPattern supports exact match and a single trailing '*' prefix wildcard.
//
// Deliberately not path.Match or a regexp: policy patterns govern who may spend
// money on what, so the matching rule needs to be one a reader can hold in
// their head and cannot accidentally over-match.
func matchPattern(pattern, id string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(id, pattern[:len(pattern)-1])
	}
	return pattern == id
}
