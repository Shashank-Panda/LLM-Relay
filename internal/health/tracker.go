// Package health tracks how endpoints are actually behaving, so the router can
// stop selecting ones that are failing.
//
// Two signals, produced at the same observation point because they come from
// the same event — a completed attempt:
//
//   - A circuit breaker per (endpoint, credential), which removes a failing
//     endpoint from routing entirely.
//   - A latency EWMA, which is the `latency` scoring dimension. The scorer has
//     always read it; until now nothing wrote it, so every endpoint scored
//     identically on latency and the dimension was weight spent on nothing.
//
// Everything here is out-of-band. The router reads a snapshot and never probes:
// a live probe inside routing would make Route impure and put a network round
// trip on the hot path to freshen a signal that moves on the order of seconds.
//
// Per-process, and that is a stated trade rather than an oversight
// (architecture §7). Behind N replicas each instance learns about a failing
// endpoint independently, so an outage is detected up to N times and the
// effective error-rate threshold is per-instance. Shared breaker state would
// cost a network round trip on every request to save a handful of failed calls.
package health

import (
	"sync"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// State is a breaker's position in its cycle.
type State string

const (
	// StateClosed is the normal condition: traffic flows, failures are counted.
	StateClosed State = "closed"

	// StateOpen means the endpoint is removed from routing. It got there by
	// failing often enough over a rolling window, not by failing once.
	StateOpen State = "open"

	// StateHalfOpen admits a bounded number of probes to find out whether the
	// endpoint recovered. Without the bound, every waiting request probes at
	// once and a recovering provider is knocked straight back over.
	StateHalfOpen State = "half_open"
)

// Config tunes the breaker.
type Config struct {
	// Window is the rolling period failures are counted over. Rolling rather
	// than cumulative: an endpoint that failed a hundred times an hour ago and
	// has worked since is healthy, and a counter with no horizon would never
	// let it back.
	Window time.Duration

	// MinRequests is the sample size below which the ratio is not consulted.
	// Two failures out of two is a 100% error rate and means nothing.
	MinRequests int

	// FailureRatio trips the breaker when reached over the window.
	FailureRatio float64

	// OpenFor is how long an open breaker waits before admitting a probe.
	OpenFor time.Duration

	// HalfOpenProbes is how many attempts may run while probing. One success
	// closes the breaker; one failure re-opens it.
	HalfOpenProbes int

	// LatencyAlpha weights the newest sample in the EWMA. Higher follows
	// changes faster and is noisier.
	LatencyAlpha float64

	// EscalationMinSamples is how many served requests an endpoint needs before
	// its escalation rate is allowed to affect routing. One bad answer out of
	// one is not a quality signal.
	EscalationMinSamples int

	// MaxQualityPenalty bounds how far observation may push an asserted score
	// down. Unbounded, a burst of escalations drives an endpoint's effective
	// quality to zero and removes it from every route carrying a floor —
	// converting a quality signal into an outage.
	MaxQualityPenalty float64

	// Now is injected so the state machine is testable without sleeping.
	Now func() time.Time
}

func DefaultConfig() Config {
	return Config{
		// Thirty seconds of history at the SLO's traffic is thousands of
		// requests, and short enough that a recovered endpoint returns to
		// service in well under a minute.
		Window:      30 * time.Second,
		MinRequests: 20,
		// Half. Deliberately not a low threshold: an endpoint failing 10% of
		// requests is degraded but still useful, and removing it entirely
		// concentrates that load onto whatever remains.
		FailureRatio:   0.5,
		OpenFor:        10 * time.Second,
		HalfOpenProbes: 3,
		LatencyAlpha:   0.2,
		// Fifty completed requests before the rate means anything, and at most
		// 0.3 of penalty. Against the 2% SLO, an endpoint escalating at 20%
		// loses 0.2 of asserted quality — enough to drop it below a floor set
		// anywhere near its claimed score, and not enough to erase an endpoint
		// over one bad afternoon.
		EscalationMinSamples: 50,
		MaxQualityPenalty:    0.3,
		Now:                  time.Now,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Window <= 0 {
		c.Window = d.Window
	}
	if c.MinRequests <= 0 {
		c.MinRequests = d.MinRequests
	}
	if c.FailureRatio <= 0 || c.FailureRatio > 1 {
		c.FailureRatio = d.FailureRatio
	}
	if c.OpenFor <= 0 {
		c.OpenFor = d.OpenFor
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = d.HalfOpenProbes
	}
	if c.LatencyAlpha <= 0 || c.LatencyAlpha > 1 {
		c.LatencyAlpha = d.LatencyAlpha
	}
	if c.EscalationMinSamples <= 0 {
		c.EscalationMinSamples = d.EscalationMinSamples
	}
	if c.MaxQualityPenalty <= 0 || c.MaxQualityPenalty > 1 {
		c.MaxQualityPenalty = d.MaxQualityPenalty
	}
	if c.Now == nil {
		c.Now = d.Now
	}
	return c
}

// Key identifies a breaker.
//
// Per (endpoint, credential), because a rate limit belongs to an API key rather
// than to a company: one tenant's key being throttled says nothing about
// another's against the same model. Until the credential resolver lands
// (ADR-0004) there is one credential per endpoint and the distinction is
// latent — but keying it correctly now means the resolver does not arrive to
// find a breaker that has to be re-keyed while it is load-bearing.
type Key struct {
	Endpoint   string
	Credential string
}

// Tracker holds every endpoint's breaker and latency estimate.
type Tracker struct {
	cfg Config

	mu       sync.Mutex
	breakers map[Key]*breaker

	// latency is keyed by endpoint alone. It describes how fast a model
	// answers, which is a property of the model and not of whose key paid
	// for it.
	latency map[string]time.Duration

	// escalations tracks how often each endpoint's output failed a validity
	// check, as a share of what it served. Keyed by endpoint for the same
	// reason latency is: it describes the model, not the credential.
	escalations map[string]*ratio
}

// ratio is a decaying count of events out of trials.
//
// Decaying rather than cumulative, for the reason the breaker's window is: an
// endpoint that escalated heavily last month and has been clean since is not a
// bad endpoint, and a counter with no horizon holds it against them forever.
// Halving both terms preserves the rate while letting recent traffic dominate.
type ratio struct {
	events float64
	trials float64
}

const ratioDecayAt = 2048

func (r *ratio) observe(hit bool) {
	r.trials++
	if hit {
		r.events++
	}
	if r.trials >= ratioDecayAt {
		r.events /= 2
		r.trials /= 2
	}
}

func (r *ratio) rate() float64 {
	if r.trials <= 0 {
		return 0
	}
	return r.events / r.trials
}

func New(cfg Config) *Tracker {
	return &Tracker{
		cfg:         cfg.withDefaults(),
		breakers:    map[Key]*breaker{},
		latency:     map[string]time.Duration{},
		escalations: map[string]*ratio{},
	}
}

// ObserveServed records that an endpoint produced an answer, and whether that
// answer had to be escalated away from.
//
// Both facts in one call, because the rate needs both terms and sourcing them
// from two call sites is how a denominator drifts away from its numerator.
// escalated=false on every ordinary request is the common path and is exactly
// what keeps the rate meaningful.
func (t *Tracker) ObserveServed(endpoint string, escalated bool) {
	if t == nil || endpoint == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	r, ok := t.escalations[endpoint]
	if !ok {
		r = &ratio{}
		t.escalations[endpoint] = r
	}
	r.observe(escalated)
}

// qualityPenalty converts an observed escalation rate into a quality
// adjustment. The caller holds the lock.
//
// The penalty *is* the rate, bounded. No curve and no coefficient: an endpoint
// returning invalid output on a fifth of requests has an effective quality 0.2
// below what the catalog claims, which is a statement anybody can check against
// the ledger. A tuned multiplier would make the number unexplainable in exchange
// for accuracy nobody can demonstrate.
func (t *Tracker) qualityPenalty(endpoint string) float64 {
	r, ok := t.escalations[endpoint]
	if !ok || r.trials < float64(t.cfg.EscalationMinSamples) {
		return 0
	}
	p := r.rate()
	if p > t.cfg.MaxQualityPenalty {
		return t.cfg.MaxQualityPenalty
	}
	return p
}

// Allow reports whether an attempt against this key may proceed.
//
// Called at execution as well as at routing, and the two are not redundant. The
// router filters on a snapshot that may be a few milliseconds old; this is what
// admits a half-open probe and refuses the request behind it, which is the
// difference between finding out whether an endpoint recovered and finding out
// by sending it everything at once.
func (t *Tracker) Allow(k Key) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.breakerFor(k).allow(t.cfg, t.cfg.Now())
}

// Observe records the outcome of one attempt.
//
// class is the provider error class, or empty on success. Only RetrySame and
// RetryOther count as failures: a Terminal error is the caller's malformed
// request and a Cancelled one is the caller leaving, and letting either trip
// the breaker would let one tenant's bad client remove an endpoint from
// everybody's routing.
func (t *Tracker) Observe(k Key, class provider.ErrorClass, latency time.Duration) {
	if t == nil {
		return
	}
	now := t.cfg.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	b := t.breakerFor(k)
	switch {
	case class == "":
		b.succeed(t.cfg, now)
		t.observeLatency(k.Endpoint, latency)
	case countsAsFailure(class):
		b.fail(t.cfg, now)
	default:
		// Not a failure and not a success worth timing: a Terminal error tells
		// us nothing about the endpoint's health, and a cancelled request was
		// cut short so its duration is the client's, not the provider's.
		b.ignore(t.cfg, now)
	}
}

func countsAsFailure(c provider.ErrorClass) bool {
	return c == provider.ClassRetrySame || c == provider.ClassRetryOther
}

// observeLatency folds a new sample into the endpoint's EWMA.
//
// The caller passes time-to-first-token for streams and total duration
// otherwise. Mixing the two would be wrong in the same direction every time:
// a stream's total duration is dominated by how long the answer is, which is a
// property of the request rather than of the endpoint.
func (t *Tracker) observeLatency(endpoint string, sample time.Duration) {
	if sample <= 0 {
		return
	}
	prev, ok := t.latency[endpoint]
	if !ok {
		t.latency[endpoint] = sample
		return
	}
	a := t.cfg.LatencyAlpha
	t.latency[endpoint] = time.Duration(a*float64(sample) + (1-a)*float64(prev))
}

func (t *Tracker) breakerFor(k Key) *breaker {
	b, ok := t.breakers[k]
	if !ok {
		b = &breaker{}
		t.breakers[k] = b
	}
	return b
}

// Snapshot renders the current health for the router.
//
// Keyed by endpoint, collapsing every credential: an endpoint is unavailable if
// any breaker on it is open. That is the conservative direction, and it matches
// domain.Health's own shape — which stays endpoint-keyed until ADR-0004's
// resolver makes multiple credentials per endpoint a real configuration rather
// than a hypothetical one.
func (t *Tracker) Snapshot() *domain.Health {
	if t == nil {
		return nil
	}
	now := t.cfg.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	out := make(map[string]domain.EndpointHealth, len(t.latency))
	for id, l := range t.latency {
		out[id] = domain.EndpointHealth{LatencyEWMA: l}
	}
	for id := range t.escalations {
		h := out[id]
		h.QualityPenalty = t.qualityPenalty(id)
		out[id] = h
	}
	for k, b := range t.breakers {
		h := out[k.Endpoint]
		if b.state(t.cfg, now) == StateOpen {
			h.CircuitOpen = true
		}
		out[k.Endpoint] = h
	}
	return &domain.Health{Endpoints: out}
}

// Report is one breaker's condition, for the admin endpoint.
type Report struct {
	Endpoint   string  `json:"endpoint"`
	Credential string  `json:"credential,omitempty"`
	State      State   `json:"state"`
	Requests   int     `json:"window_requests"`
	Failures   int     `json:"window_failures"`
	LatencyMS  float64 `json:"latency_ewma_ms"`

	// EscalationRate and QualityPenalty are ADR-0009's control loop made
	// visible. An operator asking why a route stopped selecting an endpoint it
	// used to prefer gets the answer here rather than by inference.
	EscalationRate float64 `json:"escalation_rate"`
	QualityPenalty float64 `json:"quality_penalty"`
}

// Reports lists every tracked key, so an operator asking why an endpoint is not
// being selected gets an answer without attaching a debugger.
func (t *Tracker) Reports() []Report {
	if t == nil {
		return nil
	}
	now := t.cfg.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]Report, 0, len(t.breakers))
	for k, b := range t.breakers {
		b.trim(t.cfg, now)
		reqs, fails := b.counts()
		rep := Report{
			Endpoint:       k.Endpoint,
			Credential:     k.Credential,
			State:          b.state(t.cfg, now),
			Requests:       reqs,
			Failures:       fails,
			LatencyMS:      float64(t.latency[k.Endpoint]) / float64(time.Millisecond),
			QualityPenalty: t.qualityPenalty(k.Endpoint),
		}
		if r, ok := t.escalations[k.Endpoint]; ok {
			rep.EscalationRate = r.rate()
		}
		out = append(out, rep)
	}
	return out
}

// Penalties is the current quality revision per endpoint, for the metrics gauge.
func (t *Tracker) Penalties() map[string]float64 {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make(map[string]float64, len(t.escalations))
	for id := range t.escalations {
		out[id] = t.qualityPenalty(id)
	}
	return out
}

// Open counts breakers that are currently removing an endpoint from routing,
// for the metrics gauge.
func (t *Tracker) Open() int {
	if t == nil {
		return 0
	}
	now := t.cfg.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	n := 0
	for _, b := range t.breakers {
		if b.state(t.cfg, now) == StateOpen {
			n++
		}
	}
	return n
}
