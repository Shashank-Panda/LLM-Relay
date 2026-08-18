package meter

import (
	"testing"
)

func outputRecord(route string, tokens int) Record {
	return Record{RouteName: route, Outcome: OutcomeSuccess, OutputTokens: tokens}
}

func feed(s *RouteStats, route string, tokens, n int) {
	for range n {
		s.Write(outputRecord(route, tokens))
	}
}

func TestP95NeedsEnoughHistory(t *testing.T) {
	s := NewRouteStats(50)
	feed(s, "r", 300, 49)

	if _, ok := s.OutputP95("r"); ok {
		t.Error("reported a p95 from 49 samples; a ceiling built on that caps a route " +
			"at whatever its first few answers happened to be")
	}

	s.Write(outputRecord("r", 300))
	if _, ok := s.OutputP95("r"); !ok {
		t.Error("still no p95 at the configured minimum")
	}
}

func TestUnknownRouteHasNoOpinion(t *testing.T) {
	s := NewRouteStats(1)
	if p95, ok := s.OutputP95("never-seen"); ok {
		t.Errorf("OutputP95 = (%d, true) for an unseen route; absent history must read as "+
			"\"no basis\", never as a small number", p95)
	}
}

func TestP95CoversTheTail(t *testing.T) {
	s := NewRouteStats(10)
	// Ninety short answers and ten long ones, so more than 5% of traffic sits
	// above the short bucket and the 95th percentile genuinely lands in the
	// tail. The reported figure must cover those long answers rather than cut
	// them off — this number becomes an output ceiling.
	feed(s, "r", 100, 90)
	feed(s, "r", 3000, 10)

	p95, ok := s.OutputP95("r")
	if !ok {
		t.Fatal("no p95")
	}
	if p95 < 3000 {
		t.Errorf("p95 = %d, want at least 3000: a ceiling below the observations it "+
			"was derived from truncates the responses it was meant to accommodate", p95)
	}
}

func TestP95IgnoresATailThinnerThanFivePercent(t *testing.T) {
	s := NewRouteStats(10)
	// The complement of the case above, and the reason the quantile is not just
	// "the largest thing we saw": one 30,000-token outlier in a hundred requests
	// must not set the ceiling for the other ninety-nine.
	feed(s, "r", 100, 99)
	feed(s, "r", 30000, 1)

	p95, ok := s.OutputP95("r")
	if !ok {
		t.Fatal("no p95")
	}
	if p95 > 1024 {
		t.Errorf("p95 = %d; a single outlier dragged the ceiling up for the whole route", p95)
	}
}

func TestOverflowHasNoComputableCeiling(t *testing.T) {
	s := NewRouteStats(10)
	// A route that regularly produces more than the largest bucket is precisely
	// the one that must not be capped.
	feed(s, "r", 200_000, 100)

	if p95, ok := s.OutputP95("r"); ok {
		t.Errorf("OutputP95 = (%d, true) for a route above every bucket edge", p95)
	}
}

func TestOnlySuccessfulProviderCallsAreObserved(t *testing.T) {
	s := NewRouteStats(1)

	// An error produced no answer; a cancelled stream produced a prefix of one.
	// Counting a truncated length would drag the p95 down every time a client
	// hung up, which lowers the ceiling, which truncates more responses — a
	// feedback loop that runs the wrong way on its own.
	s.Write(Record{RouteName: "r", Outcome: OutcomeError, OutputTokens: 10})
	s.Write(Record{RouteName: "r", Outcome: OutcomeCancelled, OutputTokens: 10})
	// A cache hit is a replay. Counting it again would let one popular prompt
	// reshape the route's whole distribution in proportion to how often it
	// repeats.
	s.Write(Record{RouteName: "r", Outcome: OutcomeSuccess, OutputTokens: 10, CacheHit: true})
	// No route to attribute to.
	s.Write(Record{Outcome: OutcomeSuccess, OutputTokens: 10})

	if _, ok := s.OutputP95("r"); ok {
		t.Error("observed a record that describes no completed generation")
	}

	s.Write(outputRecord("r", 500))
	if _, ok := s.OutputP95("r"); !ok {
		t.Error("a successful record was not observed")
	}
}

func TestDistributionFollowsRecentTraffic(t *testing.T) {
	s := NewRouteStats(10)

	// A workload that used to produce short answers and now produces long ones:
	// a prompt got rewritten, a feature shipped. The statistic has to re-learn,
	// or the ceiling stays wrong forever.
	feed(s, "r", 100, decayAt)
	before, _ := s.OutputP95("r")

	feed(s, "r", 8000, decayAt)
	after, _ := s.OutputP95("r")

	if after <= before {
		t.Errorf("p95 went from %d to %d after the workload doubled in length; "+
			"the statistic is averaging over all history instead of following traffic",
			before, after)
	}
}

func TestRoutesListsOnlyOptimizableRoutes(t *testing.T) {
	s := NewRouteStats(20)
	feed(s, "ready", 400, 25)
	feed(s, "cold", 400, 3)

	routes := s.Routes()
	if _, ok := routes["ready"]; !ok {
		t.Error("a route with enough history is missing from the report")
	}
	if _, ok := routes["cold"]; ok {
		t.Error("a route without enough history is listed as optimizable, which is " +
			"exactly the confusion this report exists to resolve")
	}
}

func TestNilRouteStatsIsSafe(t *testing.T) {
	var s *RouteStats
	s.Write(outputRecord("r", 100))
	if _, ok := s.OutputP95("r"); ok {
		t.Error("a nil RouteStats claimed to have an opinion")
	}
	if len(s.Routes()) != 0 {
		t.Error("a nil RouteStats reported routes")
	}
}

func TestConcurrentObservation(t *testing.T) {
	s := NewRouteStats(10)

	done := make(chan struct{})
	for i := range 8 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := range 200 {
				s.Write(outputRecord("r", 100+j))
				if j%10 == 0 {
					s.OutputP95("r")
				}
			}
		}(i)
	}
	for range 8 {
		<-done
	}

	if _, ok := s.OutputP95("r"); !ok {
		t.Error("no p95 after 1600 concurrent observations")
	}
}
