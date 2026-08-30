package routing

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
)

func mustRoute(t *testing.T, in Input) *domain.Decision {
	t.Helper()
	d, err := Route(in)
	if err != nil {
		t.Fatalf("Route: unexpected error: %v", err)
	}
	return d
}

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f (±%g)", name, got, want, tol)
	}
}

// TestWorkedExample_MatchesDocumentation is the load-bearing test in this
// package. docs/routing.md §7 shows the arithmetic a reader can verify by hand;
// this asserts the implementation produces exactly those numbers. If it fails,
// either the code is wrong or the documentation is lying to customers.
func TestWorkedExample_MatchesDocumentation(t *testing.T) {
	d := mustRoute(t, docInput())

	t.Run("filter eliminates two candidates", func(t *testing.T) {
		if len(d.Rejected) != 2 {
			t.Fatalf("rejected %d candidates, want 2: %+v", len(d.Rejected), d.Rejected)
		}
		if r, ok := rejectionFor(d, epQwen); !ok || r.Reason != domain.RejectMissingCapability {
			t.Errorf("qwen rejection = %+v, want MissingCapability", r)
		}
		if r, ok := rejectionFor(d, epFlash); !ok || r.Reason != domain.RejectCircuitOpen {
			t.Errorf("flash rejection = %+v, want CircuitOpen", r)
		}
	})

	t.Run("ranking order", func(t *testing.T) {
		want := []string{epSonnet, epMini, epOpus}
		if got := rankedIDs(d); !reflect.DeepEqual(got, want) {
			t.Errorf("ranking = %v, want %v", got, want)
		}
	})

	t.Run("scores match the published table", func(t *testing.T) {
		for _, tc := range []struct {
			id   string
			want float64
		}{
			{epSonnet, 0.840},
			{epMini, 0.748},
			{epOpus, 0.380},
		} {
			got, ok := scoreOf(d, tc.id)
			if !ok {
				t.Fatalf("%s missing from ranking", tc.id)
			}
			approx(t, tc.id+" total", got, tc.want, 5e-4)
		}
	})

	t.Run("component breakdown for the winner", func(t *testing.T) {
		var sonnet domain.ScoredCandidate
		for _, c := range d.Ranked {
			if c.EndpointID == epSonnet {
				sonnet = c
			}
		}
		approx(t, "quality.coding", sonnet.Components["quality.coding"], 0.352, 1e-9)
		approx(t, "cost", sonnet.Components[domain.DimCost], 0.242, 5e-4)
		approx(t, "latency", sonnet.Components[domain.DimLatency], 0.145, 5e-4)
		approx(t, "cache_affinity", sonnet.Components[domain.DimCacheAffinity], 0.100, 1e-9)
	})

	t.Run("estimated costs", func(t *testing.T) {
		// (8000/1e6)*in + (1500/1e6)*out, in micro-dollars.
		for _, tc := range []struct {
			id   string
			want domain.Money
		}{
			{epOpus, 232_500},  // $0.2325
			{epSonnet, 46_500}, // $0.0465
			{epMini, 2_100},    // $0.0021
		} {
			for _, c := range d.Ranked {
				if c.EndpointID == tc.id && c.Cost != tc.want {
					t.Errorf("%s cost = %s, want %s", tc.id, c.Cost, tc.want)
				}
			}
		}
	})

	t.Run("saving is measured against the baseline", func(t *testing.T) {
		if d.Chosen != epSonnet {
			t.Fatalf("chosen = %s, want %s", d.Chosen, epSonnet)
		}
		if !d.SavingMeasured {
			t.Error("SavingMeasured = false, want true")
		}
		if d.BaselineCost != 232_500 {
			t.Errorf("BaselineCost = %s, want $0.232500", d.BaselineCost)
		}
		if d.EstimatedSaved != 186_000 {
			t.Errorf("EstimatedSaved = %s, want $0.186000", d.EstimatedSaved)
		}
		if !d.Substituted() {
			t.Error("Substituted() = false, want true (opus baseline, sonnet served)")
		}
	})
}

// TestCacheAffinity_FlipsTheDecision verifies the specific claim made in
// docs/routing.md §7: strip cache affinity out and the cheap model wins by
// 0.008, migrating the conversation off its warm prefix cache.
func TestCacheAffinity_FlipsTheDecision(t *testing.T) {
	in := docInput()
	in.Request.PreviousEndpoint = "" // no previous turn, so no affinity to award

	d := mustRoute(t, in)

	if d.Chosen != epMini {
		t.Fatalf("without affinity, chosen = %s, want %s", d.Chosen, epMini)
	}
	sonnet, _ := scoreOf(d, epSonnet)
	mini, _ := scoreOf(d, epMini)
	approx(t, "sonnet without affinity", sonnet, 0.740, 5e-4)
	approx(t, "mini", mini, 0.748, 5e-4)
	if mini <= sonnet {
		t.Errorf("expected mini (%.4f) to beat sonnet (%.4f)", mini, sonnet)
	}
}

func TestBaselineModes(t *testing.T) {
	t.Run("strict serves the baseline without scoring", func(t *testing.T) {
		in := docInput()
		in.Request.Baseline.Mode = domain.ModeStrict

		d := mustRoute(t, in)

		if d.Chosen != epOpus {
			t.Errorf("chosen = %s, want baseline %s", d.Chosen, epOpus)
		}
		if d.Substituted() {
			t.Error("strict mode substituted the model")
		}
		if len(d.Rejected) != 0 {
			t.Errorf("strict mode ran the filter: %+v", d.Rejected)
		}
		if d.EstimatedSaved != 0 {
			t.Errorf("EstimatedSaved = %s, want 0", d.EstimatedSaved)
		}
	})

	t.Run("shadow serves the baseline but records the counterfactual", func(t *testing.T) {
		in := docInput()
		in.Request.Baseline.Mode = domain.ModeShadow

		d := mustRoute(t, in)

		if d.Chosen != epOpus {
			t.Errorf("chosen = %s, want baseline %s", d.Chosen, epOpus)
		}
		if d.Counterfactual != epSonnet {
			t.Errorf("counterfactual = %s, want %s", d.Counterfactual, epSonnet)
		}
		if d.Substituted() {
			t.Error("shadow mode must not substitute")
		}
		if len(d.Ranked) == 0 {
			t.Error("shadow mode should still produce a ranking")
		}
	})

	t.Run("request mode overrides tenant policy", func(t *testing.T) {
		// This is X-Relay-Pin: strict escaping a tenant-wide optimize setting.
		in := docInput()
		in.Policy.OptimizationMode = domain.ModeOptimize
		in.Request.Baseline.Mode = domain.ModeStrict

		if d := mustRoute(t, in); d.Chosen != epOpus {
			t.Errorf("chosen = %s, want the pinned baseline %s", d.Chosen, epOpus)
		}
	})

	t.Run("policy default applies when the request says nothing", func(t *testing.T) {
		in := docInput()
		in.Request.Baseline.Mode = ""
		in.Policy.OptimizationMode = domain.ModeStrict

		if d := mustRoute(t, in); d.Chosen != epOpus {
			t.Errorf("chosen = %s, want %s under a strict tenant default", d.Chosen, epOpus)
		}
	})

	t.Run("zero value is strict", func(t *testing.T) {
		in := docInput()
		in.Request.Baseline.Mode = ""
		in.Policy = &domain.Policy{Version: "empty"}

		if d := mustRoute(t, in); d.Chosen != epOpus {
			t.Errorf("chosen = %s, want %s — nobody gets substituted by default", d.Chosen, epOpus)
		}
	})
}

// TestBaselineIsCeiling asserts that Relay cannot upsell: an endpoint costing
// more than what the caller asked for is eliminated, not ranked.
func TestBaselineIsCeiling(t *testing.T) {
	in := docInput()
	in.Request.Baseline.EndpointID = epSonnet // cheaper baseline; opus now exceeds it
	// Opus normally enters as the baseline; name it explicitly so it is still
	// evaluated once something cheaper takes that role.
	rt := in.Catalog.Routes[routeFastCoder]
	rt.Candidates = append(rt.Candidates, epOpus)

	d := mustRoute(t, in)

	r, ok := rejectionFor(d, epOpus)
	if !ok {
		t.Fatalf("opus was not rejected; ranking = %v", rankedIDs(d))
	}
	if r.Reason != domain.RejectAboveBaseline {
		t.Errorf("opus rejection = %s, want AboveBaseline", r.Reason)
	}
	if d.Chosen == epOpus {
		t.Error("served an endpoint more expensive than the baseline")
	}
}

func TestQualityFloor_IsAFilterNotAWeight(t *testing.T) {
	in := docInput()
	in.Policy.QualityFloor = map[string]float64{"coding": 0.80}

	d := mustRoute(t, in)

	r, ok := rejectionFor(d, epMini)
	if !ok {
		t.Fatalf("gpt-4o-mini (coding 0.62) survived a 0.80 floor; ranking = %v", rankedIDs(d))
	}
	if r.Reason != domain.RejectBelowQualityFloor {
		t.Errorf("rejection = %s, want BelowQualityFloor", r.Reason)
	}
	if d.Chosen != epSonnet {
		t.Errorf("chosen = %s, want %s", d.Chosen, epSonnet)
	}
}

func TestQualityFloor_OnlyAppliesToScoredDimensions(t *testing.T) {
	// A floor on a dimension this route does not rank by must not eliminate
	// anything: the score means something else entirely on another route.
	in := docInput()
	in.Policy.QualityFloor = map[string]float64{"vision": 0.99}

	d := mustRoute(t, in)

	if _, ok := rejectionFor(d, epSonnet); ok {
		t.Error("an unrelated vision floor eliminated a candidate on a coding route")
	}
	if d.Chosen != epSonnet {
		t.Errorf("chosen = %s, want %s", d.Chosen, epSonnet)
	}
}

func TestFilterReasons(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(in *Input)
		subject string
		want    domain.RejectReason
	}{
		{
			name:    "retired endpoint",
			subject: epSonnet,
			want:    domain.RejectDeprecated,
			mutate: func(in *Input) {
				ep := in.Catalog.Endpoints[epSonnet]
				ep.Lifecycle = domain.Lifecycle{Status: domain.StatusRetired, Replacement: epOpus}
			},
		},
		{
			name:    "missing capability",
			subject: epSonnet,
			want:    domain.RejectMissingCapability,
			mutate: func(in *Input) {
				in.Request.Need.Vision = true
			},
		},
		{
			name:    "context too small",
			subject: epMini,
			want:    domain.RejectContextTooSmall,
			mutate: func(in *Input) {
				in.Request.Estimate.InputTokens = 150_000
				in.Request.Estimate.MaxOutputTokens = 4_000
			},
		},
		{
			name:    "policy deny list",
			subject: epSonnet,
			want:    domain.RejectPolicyDenied,
			mutate: func(in *Input) {
				in.Policy.Deny = []string{"anthropic/claude-sonnet-*"}
			},
		},
		{
			name:    "policy allow list excludes",
			subject: epMini,
			want:    domain.RejectPolicyDenied,
			mutate: func(in *Input) {
				in.Policy.Allow = []string{"anthropic/*"}
			},
		},
		{
			name:    "data residency",
			subject: epFlash,
			want:    domain.RejectPolicyDenied,
			mutate: func(in *Input) {
				in.Catalog.Endpoints[epFlash].Deployment = "eu-west"
				in.Health.Endpoints[epFlash] = domain.EndpointHealth{LatencyEWMA: time.Second}
				in.Policy.Regions = []string{"us-east"}
			},
		},
		{
			name:    "no credential",
			subject: epMini,
			want:    domain.RejectNoCredential,
			mutate: func(in *Input) {
				in.Catalog.Endpoints[epMini].CredentialRef = ""
			},
		},
		{
			// The catalog names a ref, but this caller cannot resolve it. That
			// is the BYOK case, and answering only the previous question ranks
			// endpoints the caller cannot authenticate to above the ones they
			// can — after which the executor discovers the truth one paid
			// attempt at a time.
			name:    "credential named but unavailable",
			subject: epMini,
			want:    domain.RejectNoCredential,
			mutate: func(in *Input) {
				in.Catalog.Endpoints[epMini].CredentialRef = "openai-primary"
				in.Credentials = domain.NewCredentialSet("anthropic-primary")
			},
		},
		{
			name:    "circuit open",
			subject: epSonnet,
			want:    domain.RejectCircuitOpen,
			mutate: func(in *Input) {
				in.Health.Endpoints[epSonnet] = domain.EndpointHealth{CircuitOpen: true}
			},
		},
		{
			name:    "per-request cost cap",
			subject: epOpus,
			want:    domain.RejectBudgetExceeded,
			mutate: func(in *Input) {
				in.Policy.MaxCostPerRequest = 100_000 // $0.10; opus estimates $0.2325
			},
		},
		{
			name:    "route quality requirement",
			subject: epMini,
			want:    domain.RejectBelowQualityFloor,
			mutate: func(in *Input) {
				rt := in.Catalog.Routes[routeFastCoder]
				rt.Require = []domain.Constraint{{QualityDim: "coding", QualityMin: 0.70}}
			},
		},
		{
			name:    "unknown endpoint in route",
			subject: "does/not-exist@nowhere",
			want:    domain.RejectUnknownEndpoint,
			mutate: func(in *Input) {
				rt := in.Catalog.Routes[routeFastCoder]
				rt.Candidates = append(rt.Candidates, "does/not-exist@nowhere")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := docInput()
			tc.mutate(&in)

			d := mustRoute(t, in)

			r, ok := rejectionFor(d, tc.subject)
			if !ok {
				t.Fatalf("%s was not rejected; ranking = %v", tc.subject, rankedIDs(d))
			}
			if r.Reason != tc.want {
				t.Errorf("reason = %s, want %s (detail: %s)", r.Reason, tc.want, r.Detail)
			}
			if r.Detail == "" {
				t.Error("rejection carries no detail; 'why not X' must be answerable")
			}
		})
	}
}

func TestFallback(t *testing.T) {
	// Eliminate every candidate by demanding a capability none of them has.
	eliminateAll := func(in *Input) {
		in.Request.Need.Reasoning = true
	}

	t.Run("route fallback is served", func(t *testing.T) {
		in := docInput()
		eliminateAll(&in)

		d := mustRoute(t, in)

		if !d.UsedFallback {
			t.Error("UsedFallback = false")
		}
		if d.Chosen != epSonnet {
			t.Errorf("chosen = %s, want route fallback %s", d.Chosen, epSonnet)
		}
	})

	t.Run("baseline is served when the route declares no fallback", func(t *testing.T) {
		in := docInput()
		eliminateAll(&in)
		in.Catalog.Routes[routeFastCoder].Fallback = ""

		d := mustRoute(t, in)

		if d.Chosen != epOpus {
			t.Errorf("chosen = %s, want baseline %s", d.Chosen, epOpus)
		}
		if !d.UsedFallback {
			t.Error("UsedFallback = false")
		}
	})

	t.Run("unknown route degrades to the baseline rather than failing", func(t *testing.T) {
		in := docInput()
		in.Request.RouteName = "relay/does-not-exist"

		d := mustRoute(t, in)

		if d.Chosen != epOpus {
			t.Errorf("chosen = %s, want baseline %s — an operator's typo is not the caller's outage",
				d.Chosen, epOpus)
		}
	})

	t.Run("no candidate, no fallback, no baseline is an error", func(t *testing.T) {
		in := docInput()
		eliminateAll(&in)
		in.Catalog.Routes[routeFastCoder].Fallback = ""
		in.Request.Baseline.EndpointID = ""

		if _, err := Route(in); !errors.Is(err, ErrNoCandidate) {
			t.Errorf("err = %v, want ErrNoCandidate", err)
		}
	})
}

func TestSavingUnmeasuredWithoutBaseline(t *testing.T) {
	in := docInput()
	in.Request.Baseline.EndpointID = ""

	d := mustRoute(t, in)

	if d.SavingMeasured {
		t.Error("SavingMeasured = true with no baseline; unmeasured is not zero")
	}
	if d.EstimatedSaved != 0 {
		t.Errorf("EstimatedSaved = %s, want 0", d.EstimatedSaved)
	}
}

// TestDeterminism is the property the entire design rests on: same inputs,
// same Decision, every time. Weight maps are iterated in sorted order
// precisely so that float addition cannot reorder and perturb totals.
func TestDeterminism(t *testing.T) {
	want := mustRoute(t, docInput())
	for i := range 500 {
		got := mustRoute(t, docInput())
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d produced a different Decision:\n got %+v\nwant %+v", i, got, want)
		}
	}
}

func TestRouteRejectsBadInput(t *testing.T) {
	if _, err := Route(Input{Catalog: docCatalog()}); !errors.Is(err, ErrNoRequest) {
		t.Errorf("err = %v, want ErrNoRequest", err)
	}
	if _, err := Route(Input{Request: docRequest()}); !errors.Is(err, ErrNoCatalog) {
		t.Errorf("err = %v, want ErrNoCatalog", err)
	}
}

func TestNilPolicyAndHealthAreSafe(t *testing.T) {
	// A missing health snapshot must mean "assume available", not "refuse to
	// route" — routing is not allowed to fail closed on its own telemetry.
	in := docInput()
	in.Policy = nil
	in.Health = nil
	in.Request.Baseline.Mode = domain.ModeOptimize

	d := mustRoute(t, in)

	if d.Chosen == "" {
		t.Fatal("no endpoint chosen with nil policy and health")
	}
	if _, ok := rejectionFor(d, epFlash); ok {
		t.Error("flash was rejected despite no health snapshot saying so")
	}
}

func TestBaselineIsAlwaysACandidate(t *testing.T) {
	// Even when a route omits it, serving what the caller asked for must remain
	// an option the router has not quietly removed from itself.
	in := docInput()
	rt := in.Catalog.Routes[routeFastCoder]
	rt.Candidates = []string{epMini}
	rt.Require = nil
	in.Policy.QualityFloor = map[string]float64{"coding": 0.90} // eliminates mini

	d := mustRoute(t, in)

	if d.Chosen != epOpus {
		t.Errorf("chosen = %s, want baseline %s to have been considered", d.Chosen, epOpus)
	}
}

// TestNilCredentialSetKeepsEveryCandidate is the fail-open direction, and the
// reason this change was additive rather than a migration.
//
// A nil set means "nobody asked", not "nobody has any". Routing is not allowed
// to eliminate candidates because a signal it depends on was never supplied —
// that is failing closed on our own telemetry, which ADR-0010 forbids, and it
// would have broken every deployment that had not yet configured one.
func TestNilCredentialSetKeepsEveryCandidate(t *testing.T) {
	in := docInput()
	in.Credentials = nil
	in.Request.Baseline.Mode = domain.ModeOptimize

	d := mustRoute(t, in)

	if len(d.Ranked) == 0 {
		t.Fatal("no candidates survived with a nil credential set")
	}
	for _, r := range d.Rejected {
		if r.Reason == domain.RejectNoCredential && r.Detail != "no credential configured for this endpoint" {
			t.Errorf("%s rejected as %s (%q) with no credential set supplied",
				r.EndpointID, r.Reason, r.Detail)
		}
	}
}

// TestEmptyCredentialSetIsNotNil pins the distinction the two states carry.
//
// NewCredentialSet() with no refs is an assertion that nothing is available and
// must reject everything; nil is the absence of an assertion and must reject
// nothing. Conflating them in either direction is a bug with opposite symptoms:
// one is a gateway that refuses to route, the other is a gateway that ranks
// endpoints it cannot call.
func TestEmptyCredentialSetIsNotNil(t *testing.T) {
	in := docInput()
	in.Request.Baseline.Mode = domain.ModeOptimize
	in.Credentials = domain.NewCredentialSet()

	d, err := Route(in)
	if err == nil && len(d.Ranked) > 0 && d.Ranked[0].Reasons == nil {
		t.Fatalf("an empty credential set ranked %d candidates; it asserts that "+
			"none are usable", len(d.Ranked))
	}

	// Whatever it served, it must not have *ranked* anything as viable.
	if err == nil {
		for _, r := range d.Rejected {
			if r.Reason == domain.RejectNoCredential {
				return // at least one endpoint was correctly eliminated
			}
		}
		t.Error("no endpoint was rejected for want of a credential")
	}
}
