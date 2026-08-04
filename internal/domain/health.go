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
