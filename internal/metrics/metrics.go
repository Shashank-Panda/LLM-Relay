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

	"github.com/Shashank-Panda/relay/internal/meter"
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

	m.Tokens.WithLabelValues(r.Endpoint, "input").Add(float64(r.InputTokens))
	m.Tokens.WithLabelValues(r.Endpoint, "output").Add(float64(r.OutputTokens))
	if r.CachedInputTokens > 0 {
		m.Tokens.WithLabelValues(r.Endpoint, "cached_input").Add(float64(r.CachedInputTokens))
	}
	if r.ReasoningTokens > 0 {
		m.Tokens.WithLabelValues(r.Endpoint, "reasoning").Add(float64(r.ReasoningTokens))
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
