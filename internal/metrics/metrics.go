// Package metrics is Relay's Prometheus instrumentation.
//
// The label sets here are deliberately closed. Every label value comes from a
// catalog entry, a route name, a tenant ID, or a fixed enum — never from
// anything a caller supplies. A model string echoed into a label is an
// unbounded cardinality explosion that a customer can trigger by accident, and
// the failure mode is a Prometheus server that falls over rather than a bad
// number somewhere.
package metrics

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/optimize"
	"github.com/Shashank-Panda/relay/internal/provider"
)

const namespace = "relay"

// Metrics holds every collector. Constructed once and passed explicitly rather
// than registered globally, so tests can build an isolated registry instead of
// fighting over a package-level default.
type Metrics struct {
	Requests *prometheus.CounterVec
	Duration *prometheus.HistogramVec

	// Overhead is the part of a request that is Relay's fault, measured
	// separately from provider time. Without this split you cannot tell whether
	// you are slow or the provider is, and the SLO (<5ms p50) is unfalsifiable.
	Overhead prometheus.Histogram

	TTFT *prometheus.HistogramVec

	Tokens *prometheus.CounterVec

	// The product's headline numbers. Cost and BaselineCost must reconcile
	// against the savings ledger, so both are derived from the same Record.
	Cost         *prometheus.CounterVec
	BaselineCost *prometheus.CounterVec
	Saved        *prometheus.CounterVec

	// ShadowSaved is money that would have been saved, kept in its own metric
	// so no dashboard can accidentally add it to Saved and report a shadow
	// month as banked.
	ShadowSaved *prometheus.CounterVec

	Substitutions *prometheus.CounterVec

	// Unmeasured counts requests with no baseline. Rising means a growing share
	// of traffic is invisible to the savings report, which is the kind of gap
	// that makes a monthly figure quietly unrepresentative.
	Unmeasured *prometheus.CounterVec

	// UsageEstimated counts requests whose provider reported no usage block.
	// Their cost is a guess and this is how much of the total is guessed.
	UsageEstimated *prometheus.CounterVec

	// Optimizations counts each lever's firings, and BreakpointsInserted the
	// cache markers placed.
	//
	// BreakpointsInserted must never be read on its own. A breakpoint in the
	// wrong place is silently useless, so the only evidence the lever works is
	// relay_tokens_total{kind="cached_input"} rising alongside it (ADR-0008).
	Optimizations       *prometheus.CounterVec
	BreakpointsInserted prometheus.Counter

	// OptimizeDuration is the optimizer's own latency, against its 3 ms budget.
	OptimizeDuration prometheus.Histogram

	// CacheHits and CacheMisses cover the exact-match response cache.
	CacheHits   *prometheus.CounterVec
	CacheMisses *prometheus.CounterVec

	// Truncated counts responses stopped by an output ceiling, labelled by who
	// set it. A ceiling Relay set that fires regularly is wrong and should be
	// raised — which is only actionable if the two sources are distinguishable.
	Truncated *prometheus.CounterVec

	// Degraded counts every fail-open path taken.
	//
	// Passthrough is silent by design, which is exactly why this has to exist:
	// without it the gateway can stop optimizing entirely and go on looking
	// perfectly healthy (ADR-0010).
	Degraded *prometheus.CounterVec

	// Retries and Failovers separate the two ways a request costs more than one
	// call. A retry is the same endpoint again; a failover is a different one.
	// They have different causes and different fixes, and a single "attempts"
	// counter would hide both.
	Retries   *prometheus.CounterVec
	Failovers *prometheus.CounterVec
	Reroutes  *prometheus.CounterVec

	// AttemptFailures counts calls rather than requests, so one request that
	// retried twice and failed over contributes three observations against the
	// endpoints that actually failed. The ledger cannot answer this: a record
	// names the endpoint that *answered*, so deriving per-endpoint failures
	// from it would credit them to whichever endpoint rescued the request.
	AttemptFailures *prometheus.CounterVec

	// StreamFailuresAfterTTFT is the failure ADR-0003 knowingly does not cover:
	// past the first byte no failover is honest. Monitored on its own so the
	// size of that gap is a measurement rather than an assumption.
	StreamFailuresAfterTTFT *prometheus.CounterVec

	// BreakersOpen is how many endpoints are currently removed from routing.
	BreakersOpen prometheus.Gauge

	// Shed counts requests refused for capacity, by which gate refused them.
	Shed *prometheus.CounterVec

	// Escalations counts downgrades whose output failed a validity check, by the
	// endpoint that produced it and what was wrong with it.
	//
	// The endpoint label is the one that failed, not the baseline that rescued
	// the request — attributing it to the rescuer would point an operator at the
	// endpoint doing its job. Read as a rate against substitutions_total, this
	// is the ADR-0009 SLO: under 2% per route.
	Escalations *prometheus.CounterVec

	// DiscardedCost is money spent on answers that were thrown away. It is the
	// price of the escalation backstop, and it is included in cost_usd_total —
	// this is the part of that total that bought nothing.
	DiscardedCost *prometheus.CounterVec

	// QualityPenalty is the observed downward revision of each endpoint's
	// asserted quality. Non-zero means routing has stopped trusting the catalog
	// about that endpoint.
	QualityPenalty *prometheus.GaugeVec

	MeterDropped  prometheus.Counter
	StreamsActive prometheus.Gauge

	// lastDropped is the drop total last published, so ObserveDropped can add
	// the delta to a counter that only knows how to increase.
	lastDropped atomic.Uint64
}

// New builds and registers the collectors.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "requests_total",
			Help: "Requests served, by route, endpoint and outcome.",
		}, []string{"route", "endpoint", "outcome"}),

		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "request_duration_seconds",
			Help: "End-to-end request duration including provider time.",
			// Provider calls run from a hundred milliseconds to minutes, so the
			// buckets span four orders of magnitude rather than the default's
			// web-request range.
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"route", "streaming"}),

		Overhead: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "gateway_overhead_seconds",
			Help: "Request duration minus provider time: the part that is Relay's fault.",
			// Sub-millisecond resolution, because the SLO is p50 < 5ms and
			// p99 < 25ms. Default buckets start at 5ms and would put the entire
			// target range in one bucket.
			Buckets: []float64{.0001, .00025, .0005, .001, .0025, .005, .01, .025, .05, .1, .25},
		}),

		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "ttft_seconds",
			Help:    "Time to first token on streaming requests.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 4, 8, 16},
		}, []string{"endpoint"}),

		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "tokens_total",
			Help: "Tokens reported by providers, by kind.",
		}, []string{"endpoint", "kind"}),

		Cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "cost_usd_total",
			Help: "Cost of served requests, from provider-reported usage.",
		}, []string{"tenant", "endpoint"}),

		BaselineCost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "baseline_cost_usd_total",
			Help: "What the same tokens would have cost at the baseline endpoint.",
		}, []string{"tenant"}),

		Saved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "saved_usd_total",
			Help: "Money actually saved: baseline cost minus served cost.",
		}, []string{"tenant"}),

		ShadowSaved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "shadow_saved_usd_total",
			Help: "Money that WOULD have been saved under optimize mode. " +
				"Not saved. Never add this to relay_saved_usd_total.",
		}, []string{"tenant"}),

		Substitutions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "substitutions_total",
			Help: "Requests served by an endpoint other than the one asked for.",
		}, []string{"tenant", "baseline", "served"}),

		Unmeasured: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "unmeasured_requests_total",
			Help: "Requests with no baseline to price against, and so absent from every savings figure.",
		}, []string{"tenant"}),

		UsageEstimated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "usage_estimated_total",
			Help: "Requests whose provider reported no usage block, so their cost is estimated.",
		}, []string{"endpoint"}),

		Optimizations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "optimizations_total",
			Help: "Request optimizations applied, by lever.",
		}, []string{"tenant", "lever"}),

		BreakpointsInserted: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "cache_breakpoints_inserted_total",
			Help: "Cache markers placed. Effort, not effect: read against " +
				"relay_tokens_total{kind=\"cached_input\"}, never on its own.",
		}),

		OptimizeDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "optimize_duration_seconds",
			Help: "Time spent in the optimizer, against its 3ms budget.",
			// Centred on the budget rather than on web-request latencies: the
			// interesting question is how close to 3ms the p99 runs.
			Buckets: []float64{.00001, .000025, .00005, .0001, .00025, .0005, .001, .003, .01},
		}),

		CacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "cache_hits_total",
			Help: "Responses served from the exact-match response cache.",
		}, []string{"tenant", "route"}),

		CacheMisses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "cache_misses_total",
			Help: "Cacheable requests with no live entry.",
		}, []string{"tenant", "route"}),

		Truncated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "output_truncated_total",
			Help: "Responses that stopped at an output ceiling, by who set the ceiling.",
		}, []string{"endpoint", "ceiling"}),

		Degraded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "degraded_total",
			Help: "Fail-open paths taken. Non-zero means part of Relay is not working " +
				"and nothing else will say so.",
		}, []string{"component", "reason"}),

		Retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "retries_total",
			Help: "Repeat attempts against the same endpoint, labelled with the endpoint " +
				"that failed. Every one is a second charge.",
		}, []string{"endpoint", "class"}),

		AttemptFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "attempt_failures_total",
			Help: "Failed provider calls, by the endpoint that failed and the error class. " +
				"Unlike requests_total this counts calls, so one request can appear several times.",
		}, []string{"endpoint", "class"}),

		Failovers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "failovers_total",
			Help: "Requests that abandoned an endpoint and were served by another.",
		}, []string{"route"}),

		Reroutes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "reroutes_total",
			Help: "Requests re-routed after a provider rejected the routing constraints. " +
				"A rising rate means the token estimate is systematically wrong.",
		}, []string{"route"}),

		StreamFailuresAfterTTFT: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "stream_failures_after_ttft_total",
			Help: "Streams that broke after the client had received content, where " +
				"failover is not available. The measured size of ADR-0003's gap.",
		}, []string{"endpoint"}),

		BreakersOpen: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "circuit_breakers_open",
			Help: "Endpoints currently removed from routing by their circuit breaker.",
		}),

		Shed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "shed_total",
			Help: "Requests refused for capacity, by the gate that refused them.",
		}, []string{"reason"}),

		Escalations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "escalations_total",
			Help: "Downgrades whose output failed a validity check and were retried on " +
				"the baseline, by the endpoint that failed and why.",
		}, []string{"route", "endpoint", "reason", "recovered"}),

		DiscardedCost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "discarded_cost_usd_total",
			Help: "Money spent on answers that were thrown away. Already inside " +
				"relay_cost_usd_total; this is the part of it that bought nothing.",
		}, []string{"tenant"}),

		QualityPenalty: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "quality_penalty",
			Help: "Observed downward revision of an endpoint's asserted quality. " +
				"Non-zero means routing has stopped trusting the catalog about it.",
		}, []string{"endpoint"}),

		MeterDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "meter_dropped_total",
			Help: "Ledger records lost to a full buffer. Non-zero means the savings report is incomplete.",
		}),

		StreamsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "streams_active",
			Help: "Streaming responses currently open.",
		}),
	}

	if reg != nil {
		reg.MustRegister(
			m.Requests, m.Duration, m.Overhead, m.TTFT, m.Tokens,
			m.Cost, m.BaselineCost, m.Saved, m.ShadowSaved,
			m.Substitutions, m.Unmeasured, m.UsageEstimated,
			m.Optimizations, m.BreakpointsInserted, m.OptimizeDuration,
			m.CacheHits, m.CacheMisses, m.Truncated, m.Degraded,
			m.Retries, m.Failovers, m.Reroutes, m.AttemptFailures, m.StreamFailuresAfterTTFT,
			m.BreakersOpen, m.Shed,
			m.Escalations, m.DiscardedCost, m.QualityPenalty,
			m.MeterDropped, m.StreamsActive,
		)
	}
	return m
}

// Write implements meter.Sink, so metrics are fed from the same Record that
// feeds the ledger.
//
// One source rather than two instrumentation call sites. The alternative is a
// dashboard and a savings report that disagree, and no way to tell which is
// right — for the product's headline number that is not an acceptable risk.
func (m *Metrics) Write(r meter.Record) {
	if m == nil {
		return
	}

	route := or(r.RouteName, "none")
	m.Requests.WithLabelValues(route, r.Endpoint, string(r.Outcome)).Inc()
	m.Duration.WithLabelValues(route, boolLabel(r.Streaming)).Observe(r.Duration.Seconds())
	m.Overhead.Observe(r.Overhead().Seconds())

	if r.Streaming && r.TTFT > 0 {
		m.TTFT.WithLabelValues(r.Endpoint).Observe(r.TTFT.Seconds())
	}

	// Tokens counts what providers reported billing for. A cache hit bought
	// none, so adding its counts here would inflate every per-token figure
	// derived from this counter — including the one that says whether cache
	// breakpoints are working.
	if !r.CacheHit {
		m.Tokens.WithLabelValues(r.Endpoint, "input").Add(float64(r.InputTokens))
		m.Tokens.WithLabelValues(r.Endpoint, "output").Add(float64(r.OutputTokens))
		if r.CachedInputTokens > 0 {
			m.Tokens.WithLabelValues(r.Endpoint, "cached_input").Add(float64(r.CachedInputTokens))
		}
		if r.ReasoningTokens > 0 {
			m.Tokens.WithLabelValues(r.Endpoint, "reasoning").Add(float64(r.ReasoningTokens))
		}
	}

	for _, lever := range r.Optimizations {
		m.Optimizations.WithLabelValues(r.Tenant, lever).Inc()
	}
	if r.Breakpoints > 0 {
		m.BreakpointsInserted.Add(float64(r.Breakpoints))
	}
	if r.CacheHit {
		m.CacheHits.WithLabelValues(r.Tenant, route).Inc()
	}
	if r.FinishReason == string(provider.FinishLength) {
		m.Truncated.WithLabelValues(r.Endpoint, ceilingSource(r)).Inc()
	}

	if r.Failovers > 0 {
		m.Failovers.WithLabelValues(route).Add(float64(r.Failovers))
	}
	if r.Rerouted {
		m.Reroutes.WithLabelValues(route).Inc()
	}
	if r.StreamFailedAfterTTFT {
		m.StreamFailuresAfterTTFT.WithLabelValues(r.Endpoint).Inc()
	}
	if r.Escalated {
		m.Escalations.WithLabelValues(
			route, r.EscalatedFrom, r.EscalationCause, boolLabel(r.EscalationRecovered)).Inc()
		m.DiscardedCost.WithLabelValues(r.Tenant).Add(r.DiscardedCost.Dollars())
	}

	m.Cost.WithLabelValues(r.Tenant, r.Endpoint).Add(r.Cost.Dollars())

	if r.UsageEstimated {
		m.UsageEstimated.WithLabelValues(r.Endpoint).Inc()
	}
	if r.Substituted {
		m.Substitutions.WithLabelValues(r.Tenant, r.BaselineID, r.Endpoint).Inc()
	}

	if !r.SavingMeasured {
		m.Unmeasured.WithLabelValues(r.Tenant).Inc()
		return
	}

	m.BaselineCost.WithLabelValues(r.Tenant).Add(r.BaselineCost.Dollars())

	// Counters must not decrease. A negative saving is possible — a fallback to
	// a dearer endpoint, and in Phase 4 an escalation — and adding it would make
	// the counter go backwards, which Prometheus reads as a process restart and
	// which corrupts every rate() over the window.
	if r.Saved > 0 {
		m.Saved.WithLabelValues(r.Tenant).Add(r.Saved.Dollars())
	}
	if r.ShadowSaved > 0 {
		m.ShadowSaved.WithLabelValues(r.Tenant).Add(r.ShadowSaved.Dollars())
	}
}

// ObserveDropped publishes the meter's cumulative drop count.
//
// A counter rather than a gauge, because this is a monotonic total and a gauge
// would make rate() unusable on the one signal that says the savings ledger is
// incomplete. prometheus.Counter has no Set, so the delta since the last
// publish is added — lastDropped tracks it rather than reading the collector
// back, which would need a DTO round trip for a number we already have.
func (m *Metrics) ObserveDropped(total uint64) {
	if m == nil {
		return
	}
	prev := m.lastDropped.Swap(total)
	if total > prev {
		m.MeterDropped.Add(float64(total - prev))
	}
}

// ObserveOptimize records one optimization pass, including the ones that did
// nothing.
//
// Duration is recorded even for a pass with no levers enabled, because the
// question the histogram answers is "what does the optimizer cost us", and a
// sample set drawn only from passes that did work would answer a different one.
func (m *Metrics) ObserveOptimize(res optimize.Result) {
	if m == nil {
		return
	}
	m.OptimizeDuration.Observe(res.Elapsed.Seconds())
	if res.Degraded() {
		m.Degraded.WithLabelValues("optimizer", string(res.Outcome)).Inc()
	}
}

// ObserveAttempt records one provider call as it happens.
//
// Per attempt rather than per request, because that is the only place the
// failing endpoint is still known. A retry counted against the ledger record
// would be labelled with the endpoint that eventually answered — pointing an
// operator at the healthy provider during an incident caused by the broken one.
func (m *Metrics) ObserveAttempt(endpoint string, retry int, class string) {
	if m == nil {
		return
	}
	if class != "" {
		m.AttemptFailures.WithLabelValues(endpoint, class).Inc()
	}
	if retry > 0 {
		m.Retries.WithLabelValues(endpoint, or(class, "recovered")).Inc()
	}
}

// ObserveDegraded counts one fail-open path being taken.
//
// The metric ADR-0010 makes non-optional. Passthrough is silent by design — the
// customer sees a normal response — so without this, Relay can be optimizing
// nothing, saving nothing, and still look perfectly healthy on every other
// dashboard.
func (m *Metrics) ObserveDegraded(component, reason string) {
	if m == nil {
		return
	}
	m.Degraded.WithLabelValues(component, reason).Inc()
}

// ObserveQualityPenalties publishes the observed quality revisions.
//
// A gauge has to be pushed, and nothing else observes these from outside a
// request. Without it the control loop is invisible: routing quietly stops
// selecting an endpoint and nothing anywhere says why.
func (m *Metrics) ObserveQualityPenalties(byEndpoint map[string]float64) {
	if m == nil {
		return
	}
	for id, p := range byEndpoint {
		m.QualityPenalty.WithLabelValues(id).Set(p)
	}
}

// ObserveBreakers publishes how many endpoints are currently unavailable.
func (m *Metrics) ObserveBreakers(open int) {
	if m == nil {
		return
	}
	m.BreakersOpen.Set(float64(open))
}

// ObserveCacheMiss records a cacheable request that found no entry. Hits are
// recorded from the ledger record, where the tenant and route are already known.
func (m *Metrics) ObserveCacheMiss(tenant, route string) {
	if m == nil {
		return
	}
	m.CacheMisses.WithLabelValues(tenant, or(route, "none")).Inc()
}

// ceilingSource attributes a truncation to whoever set the ceiling.
//
// The distinction is the whole reason the metric exists. A caller's own
// max_tokens firing is the caller getting what they asked for; Relay's ceiling
// firing is Relay cutting off an answer somebody wanted, and only the second one
// is a bug.
func ceilingSource(r meter.Record) string {
	for _, lever := range r.Optimizations {
		if lever == domain.LeverMaxTokens {
			return "relay"
		}
	}
	return "caller"
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Ensure the sink contract is satisfied at compile time rather than at the one
// call site that wires it up.
var _ meter.Sink = (*Metrics)(nil)
