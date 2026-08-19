package health

import (
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// clock is a hand-advanced time source.
//
// The breaker is a state machine whose transitions are driven entirely by
// elapsed time. Testing it against the wall clock would mean sleeping through
// every cool-down, which makes the suite slow enough that somebody eventually
// shortens the timings until the test stops proving anything.
type clock struct{ t time.Time }

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func testConfig(c *clock) Config {
	return Config{
		Window:         time.Minute,
		MinRequests:    4,
		FailureRatio:   0.5,
		OpenFor:        10 * time.Second,
		HalfOpenProbes: 2,
		Now:            c.now,
	}
}

var key = Key{Endpoint: "openai/cheap@us", Credential: "openai-primary"}

func failN(t *Tracker, k Key, n int) {
	for range n {
		t.Observe(k, provider.ClassRetrySame, 0)
	}
}

func succeedN(t *Tracker, k Key, n int) {
	for range n {
		t.Observe(k, "", 100*time.Millisecond)
	}
}

func stateOf(t *testing.T, tr *Tracker, k Key) State {
	t.Helper()
	for _, r := range tr.Reports() {
		if r.Endpoint == k.Endpoint && r.Credential == k.Credential {
			return r.State
		}
	}
	return StateClosed
}

func TestBreakerNeedsEnoughEvidence(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))

	// Three failures out of three is a 100% error rate and means nothing. A
	// breaker that trips on it removes an endpoint from routing on the strength
	// of a coincidence.
	failN(tr, key, 3)

	if !tr.Allow(key) {
		t.Error("breaker opened below the minimum sample size")
	}
	if got := stateOf(t, tr, key); got != StateClosed {
		t.Errorf("state = %q, want %q", got, StateClosed)
	}
}

func TestBreakerOpensOnSustainedFailure(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))

	failN(tr, key, 4)

	if tr.Allow(key) {
		t.Fatal("breaker stayed closed at 100% failure over the minimum sample")
	}
	if got := stateOf(t, tr, key); got != StateOpen {
		t.Errorf("state = %q, want %q", got, StateOpen)
	}
	// And the router sees it, which is the point: an open endpoint is filtered
	// out during routing rather than failed during execution, so the recorded
	// ranking stays honest about what was available.
	if !tr.Snapshot().For(key.Endpoint).CircuitOpen {
		t.Error("snapshot does not report the endpoint as unavailable")
	}
}

func TestPartialFailureDoesNotOpen(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))

	// A third of requests failing is degraded, not dead. Removing it entirely
	// concentrates that load onto whatever remains, which is how one struggling
	// provider takes the others down with it.
	failN(tr, key, 2)
	succeedN(tr, key, 4)

	if !tr.Allow(key) {
		t.Errorf("breaker opened at a 33%% error rate against a 50%% threshold")
	}
}

func TestOnlyEndpointFailuresCount(t *testing.T) {
	classes := map[string]provider.ErrorClass{
		// A malformed request fails identically at every provider. Letting it
		// trip the breaker lets one tenant's broken client remove an endpoint
		// from everybody's routing.
		"terminal":  provider.ClassTerminal,
		"cancelled": provider.ClassCancelled,
		// A context overflow is a statement about the routing constraints, not
		// about the endpoint's health.
		"reroute": provider.ClassReroute,
	}

	for name, class := range classes {
		t.Run(name, func(t *testing.T) {
			c := newClock()
			tr := New(testConfig(c))

			for range 20 {
				tr.Observe(key, class, 0)
			}
			if !tr.Allow(key) {
				t.Errorf("%s errors tripped the breaker", name)
			}
		})
	}
}

func TestIgnoredErrorsDoNotDiluteRealOnes(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))

	// The subtle half of the rule above. Ignoring a Terminal error must mean
	// leaving it out of the window entirely — counting it as a *success* would
	// let a flood of 400s hold a genuinely broken endpoint's error ratio below
	// the threshold and keep it in rotation.
	for range 20 {
		tr.Observe(key, provider.ClassTerminal, 0)
	}
	failN(tr, key, 4)

	if tr.Allow(key) {
		t.Error("terminal errors diluted the failure ratio and kept a broken endpoint open")
	}
}

func TestHalfOpenAdmitsABoundedProbe(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))
	failN(tr, key, 4)

	c.add(11 * time.Second)

	// Two probes, then no more. Without the bound every request waiting behind
	// the breaker probes at once, and a recovering provider is knocked straight
	// back over by the traffic that was queued for it.
	if !tr.Allow(key) {
		t.Fatal("first probe refused after the cool-down")
	}
	if !tr.Allow(key) {
		t.Fatal("second probe refused inside the quota")
	}
	if tr.Allow(key) {
		t.Error("third probe admitted past a quota of two")
	}
	if got := stateOf(t, tr, key); got != StateOpen {
		t.Errorf("state = %q, want %q once the quota is spent", got, StateOpen)
	}
}

func TestSuccessfulProbeClosesTheBreaker(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))
	failN(tr, key, 4)

	c.add(11 * time.Second)
	tr.Allow(key)
	tr.Observe(key, "", 50*time.Millisecond)

	if !tr.Allow(key) {
		t.Fatal("breaker did not close after a clean probe")
	}
	// A full reset, not merely a close. The window still held the failures that
	// tripped it, and carrying them forward would re-trip on the next single
	// failure and leave the endpoint flapping.
	tr.Observe(key, provider.ClassRetrySame, 0)
	if !tr.Allow(key) {
		t.Error("one failure after recovery re-opened the breaker: the window was not reset")
	}
}

func TestFailedProbeRestartsTheCooldown(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))
	failN(tr, key, 4)

	c.add(11 * time.Second)
	tr.Allow(key)
	tr.Observe(key, provider.ClassRetrySame, 0)

	if tr.Allow(key) {
		t.Fatal("a failed probe left the breaker admitting traffic")
	}
	// A persistently broken endpoint is retried on a fixed interval rather than
	// continuously.
	c.add(11 * time.Second)
	if !tr.Allow(key) {
		t.Error("the cool-down did not restart from the failed probe")
	}
}

func TestFailuresFallOutOfTheWindow(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))

	failN(tr, key, 3)
	// An endpoint that failed an hour ago and has worked since is healthy. A
	// cumulative counter would never let it back.
	c.add(2 * time.Minute)
	failN(tr, key, 3)

	if !tr.Allow(key) {
		t.Error("failures from outside the window still counted toward the threshold")
	}
}

func TestBreakersAreIndependentPerCredential(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))

	a := Key{Endpoint: "shared@us", Credential: "tenant-a"}
	b := Key{Endpoint: "shared@us", Credential: "tenant-b"}

	failN(tr, a, 4)

	if tr.Allow(a) {
		t.Error("the failing credential's breaker stayed closed")
	}
	// A rate limit belongs to an API key, not to a model. One tenant being
	// throttled says nothing about another's key against the same endpoint.
	if !tr.Allow(b) {
		t.Error("one credential's failures tripped another's breaker")
	}
	// The snapshot collapses to the endpoint and takes the conservative
	// reading, because domain.Health is endpoint-keyed until ADR-0004's
	// resolver makes multiple credentials a real configuration.
	if !tr.Snapshot().For("shared@us").CircuitOpen {
		t.Error("the snapshot did not report an endpoint with one open breaker")
	}
}

func TestLatencyEWMA(t *testing.T) {
	c := newClock()
	cfg := testConfig(c)
	cfg.LatencyAlpha = 0.5
	tr := New(cfg)

	tr.Observe(key, "", 100*time.Millisecond)
	if got := tr.Snapshot().For(key.Endpoint).LatencyEWMA; got != 100*time.Millisecond {
		t.Errorf("first sample = %v, want it taken verbatim", got)
	}

	tr.Observe(key, "", 200*time.Millisecond)
	if got := tr.Snapshot().For(key.Endpoint).LatencyEWMA; got != 150*time.Millisecond {
		t.Errorf("EWMA = %v, want 150ms at alpha 0.5", got)
	}
}

func TestFailedAttemptsDoNotSkewLatency(t *testing.T) {
	c := newClock()
	tr := New(testConfig(c))

	tr.Observe(key, "", 500*time.Millisecond)
	// A connection refused in a microsecond is not an endpoint that answers in
	// a microsecond. Timing failures would make a completely broken endpoint
	// the fastest one on the route, and the scorer would select it.
	tr.Observe(key, provider.ClassRetrySame, 0)
	tr.Observe(key, provider.ClassCancelled, 0)

	if got := tr.Snapshot().For(key.Endpoint).LatencyEWMA; got != 500*time.Millisecond {
		t.Errorf("EWMA = %v, want 500ms — failures must not be timed", got)
	}
}

func TestNilTrackerAllowsEverything(t *testing.T) {
	// Nil means no health signal, and an absent signal must read as "route
	// normally" rather than "refuse to route". That is the fail-open direction
	// ADR-0010 requires, and it is Phase 1's behaviour unchanged.
	var tr *Tracker
	if !tr.Allow(key) {
		t.Error("a nil tracker refused an attempt")
	}
	tr.Observe(key, provider.ClassRetrySame, time.Second)
	if tr.Snapshot() != nil {
		t.Error("a nil tracker produced a snapshot")
	}
	if tr.Open() != 0 {
		t.Error("a nil tracker reported open breakers")
	}
}

func TestConcurrentObservation(t *testing.T) {
	tr := New(DefaultConfig())

	done := make(chan struct{})
	for i := range 8 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			k := Key{Endpoint: "e", Credential: "c"}
			for j := range 200 {
				if j%3 == 0 {
					tr.Observe(k, provider.ClassRetrySame, 0)
				} else {
					tr.Observe(k, "", time.Millisecond)
				}
				tr.Allow(k)
				tr.Snapshot()
			}
		}(i)
	}
	for range 8 {
		<-done
	}
}

// --- Phase 5: escalation feedback ---

func serve(t *Tracker, endpoint string, n int, escalated int) {
	for i := range n {
		t.ObserveServed(endpoint, i < escalated)
	}
}

func penaltyOf(t *Tracker, endpoint string) float64 {
	return t.Snapshot().For(endpoint).QualityPenalty
}

func TestQualityPenaltyNeedsEnoughHistory(t *testing.T) {
	c := newClock()
	cfg := testConfig(c)
	cfg.EscalationMinSamples = 50
	tr := New(cfg)

	// Ten escalations out of ten is a 100% rate and is not a quality signal.
	// Acting on it would remove an endpoint from every route with a floor on the
	// strength of one bad minute.
	serve(tr, "cheap", 10, 10)
	if got := penaltyOf(tr, "cheap"); got != 0 {
		t.Errorf("penalty = %v below the minimum sample size, want 0", got)
	}

	serve(tr, "cheap", 40, 40)
	if got := penaltyOf(tr, "cheap"); got == 0 {
		t.Error("no penalty once the sample size was reached")
	}
}

func TestQualityPenaltyIsTheEscalationRate(t *testing.T) {
	c := newClock()
	cfg := testConfig(c)
	cfg.EscalationMinSamples = 10
	cfg.MaxQualityPenalty = 1
	tr := New(cfg)

	// Twenty escalations in a hundred. The penalty is the rate, with no curve
	// and no coefficient — a number anybody can recompute from the ledger.
	serve(tr, "cheap", 100, 20)

	if got := penaltyOf(tr, "cheap"); got < 0.19 || got > 0.21 {
		t.Errorf("penalty = %v, want the 0.20 escalation rate", got)
	}
}

func TestQualityPenaltyIsBounded(t *testing.T) {
	c := newClock()
	cfg := testConfig(c)
	cfg.EscalationMinSamples = 10
	cfg.MaxQualityPenalty = 0.3
	tr := New(cfg)

	// Everything escalated. Unbounded, this would drive effective quality to
	// zero and remove the endpoint from every route carrying a floor — turning
	// a quality signal into an outage.
	serve(tr, "cheap", 100, 100)

	if got := penaltyOf(tr, "cheap"); got != 0.3 {
		t.Errorf("penalty = %v, want it capped at 0.3", got)
	}
}

func TestCleanEndpointsAreNotPenalised(t *testing.T) {
	c := newClock()
	cfg := testConfig(c)
	cfg.EscalationMinSamples = 10
	tr := New(cfg)

	serve(tr, "good", 200, 0)

	if got := penaltyOf(tr, "good"); got != 0 {
		t.Errorf("penalty = %v for an endpoint that never escalated", got)
	}
	// And never a bonus. A low escalation rate is evidence of *validity*, not of
	// quality — an endpoint returning well-formed rubbish would earn one.
	if q := tr.Snapshot().For("good").EffectiveQuality(0.6); q != 0.6 {
		t.Errorf("EffectiveQuality = %v, want the asserted score unchanged", q)
	}
}

func TestPenaltyFollowsRecentBehaviour(t *testing.T) {
	c := newClock()
	cfg := testConfig(c)
	cfg.EscalationMinSamples = 10
	cfg.MaxQualityPenalty = 1
	tr := New(cfg)

	// An endpoint that was bad and has been fixed. A cumulative counter would
	// hold the old behaviour against it forever.
	serve(tr, "cheap", ratioDecayAt, ratioDecayAt)
	before := penaltyOf(tr, "cheap")

	serve(tr, "cheap", ratioDecayAt*2, 0)
	after := penaltyOf(tr, "cheap")

	if after >= before {
		t.Errorf("penalty went from %v to %v after the endpoint recovered", before, after)
	}
}

func TestEffectiveQualityClampsAtZero(t *testing.T) {
	h := domain.EndpointHealth{QualityPenalty: 0.9}
	if got := h.EffectiveQuality(0.5); got != 0 {
		t.Errorf("EffectiveQuality = %v, want 0 — below zero every floor fails "+
			"identically and the extra precision decides nothing", got)
	}
}

func TestReportsSurfaceTheControlLoop(t *testing.T) {
	c := newClock()
	cfg := testConfig(c)
	cfg.EscalationMinSamples = 10
	tr := New(cfg)

	k := Key{Endpoint: "cheap", Credential: "primary"}
	tr.Observe(k, "", time.Millisecond) // create the breaker so it is reported
	serve(tr, "cheap", 100, 25)

	var found bool
	for _, r := range tr.Reports() {
		if r.Endpoint != "cheap" {
			continue
		}
		found = true
		if r.EscalationRate < 0.24 || r.EscalationRate > 0.26 {
			t.Errorf("escalation_rate = %v, want 0.25", r.EscalationRate)
		}
		if r.QualityPenalty == 0 {
			t.Error("quality_penalty = 0; an operator asking why a route stopped " +
				"selecting this endpoint would get no answer")
		}
	}
	if !found {
		t.Error("the endpoint is missing from the report")
	}
	if p := tr.Penalties()["cheap"]; p == 0 {
		t.Error("Penalties() does not expose the revision for the metrics gauge")
	}
}
