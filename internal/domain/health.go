package domain

import "time"

// EndpointHealth is the out-of-band view of an endpoint's condition.
//
// The router reads this snapshot and never probes. Live probing inside routing
// would make Route impure and untestable, and would put a network round trip on
// the hot path to freshen a signal that changes on the order of seconds.
type EndpointHealth struct {
	// CircuitOpen removes the endpoint from routing entirely. Open endpoints
	// are filtered out rather than failed during execution, so the ranked list
	// stays honest about what was actually available.
	CircuitOpen bool

	// LatencyEWMA is observed request duration, not a probe.
	LatencyEWMA time.Duration

	// QualityPenalty is subtracted from an endpoint's asserted quality before
	// the floor is applied and before it is scored.
	//
	// This is what closes ADR-0009's control loop. Catalog quality scores are
	// operator assertions, and the risk they carry is being optimistic: an
	// endpoint claimed at 0.85 that actually returns malformed tool calls keeps
	// being selected, keeps failing validity checks, and keeps being escalated
	// to the baseline at double cost. Feeding the observed escalation rate back
	// as a penalty means routing stops choosing it without waiting for anyone to
	// read a dashboard.
	//
	// Only ever a penalty, never a bonus. A low escalation rate is not evidence
	// of quality — it is evidence of *validity*, and an endpoint returning
	// well-formed rubbish would earn a perfect score. Quality is revised down by
	// observation and up only by an operator editing the catalog.
	QualityPenalty float64
}

// EffectiveQuality applies the observed penalty to an asserted score.
//
// Clamped at zero rather than allowed to go negative: below zero it fails every
// floor identically, and the difference between "bad" and "very bad" is not
// information a filter can act on.
func (h EndpointHealth) EffectiveQuality(asserted float64) float64 {
	q := asserted - h.QualityPenalty
	if q < 0 {
		return 0
	}
	return q
}

// Health is a snapshot keyed by endpoint ID.
//
// Keying: circuit state properly belongs to (endpoint, credential), since a
// rate limit belongs to a key rather than to a company. Until the credential
// resolver lands (ADR-0004), one credential per endpoint is assumed and this
// map is keyed by endpoint alone.
type Health struct {
	Endpoints map[string]EndpointHealth
}

// For returns the health of an endpoint, or the zero value (healthy, unknown
// latency) when nothing is recorded. Nil-safe: an absent snapshot means the
// router treats every endpoint as available rather than refusing to route,
// which is the fail-open behaviour ADR-0010 requires.
func (h *Health) For(id string) EndpointHealth {
	if h == nil || h.Endpoints == nil {
		return EndpointHealth{}
	}
	return h.Endpoints[id]
}
