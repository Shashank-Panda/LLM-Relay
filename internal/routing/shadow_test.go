package routing

import (
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
)

func shadowDecision(t *testing.T) *domain.Decision {
	t.Helper()
	in := docInput()
	in.Request.Baseline.Mode = domain.ModeShadow

	d, err := Route(in)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	return d
}

// TestShadowServesTheBaseline is the promise shadow mode makes to a cautious
// customer: measure the saving, change nothing.
func TestShadowServesTheBaseline(t *testing.T) {
	d := shadowDecision(t)

	if d.Chosen != d.Baseline.EndpointID {
		t.Errorf("Chosen = %s, want the baseline %s — shadow mode must not substitute",
			d.Chosen, d.Baseline.EndpointID)
	}
	if d.Substituted() {
		t.Error("Substituted() is true in shadow mode")
	}
	// The customer was served the baseline, so they saved nothing. Reporting
	// the counterfactual saving here would claim money never saved.
	if d.EstimatedSaved != 0 {
		t.Errorf("EstimatedSaved = %s, want 0 — the baseline was served", d.EstimatedSaved)
	}
	if d.EstimatedCost != d.BaselineCost {
		t.Errorf("EstimatedCost = %s, BaselineCost = %s; they must agree when the baseline is served",
			d.EstimatedCost, d.BaselineCost)
	}
}

// TestShadowKeepsItsRanking is the regression this file exists for.
//
// serveBaseline collapses Ranked to a single synthetic entry, which is correct
// for strict mode and for a fallback because no scoring happened. Calling it in
// shadow mode threw away the ranking that produced the counterfactual — leaving
// a decision that says sonnet would have been chosen but not why, and a savings
// report with nothing underneath it.
func TestShadowKeepsItsRanking(t *testing.T) {
	d := shadowDecision(t)

	if len(d.Ranked) < 2 {
		t.Fatalf("Ranked has %d entries, want the full ranking — shadow mode's "+
			"evidence for the counterfactual was discarded", len(d.Ranked))
	}

	if d.Ranked[0].EndpointID != d.Counterfactual {
		t.Errorf("Ranked[0] = %s but Counterfactual = %s; the top of the ranking "+
			"is by definition the counterfactual", d.Ranked[0].EndpointID, d.Counterfactual)
	}

	// Scored, not synthetic. A zero total would mean serveBaseline's placeholder
	// survived.
	if d.Ranked[0].Total <= 0 {
		t.Errorf("Ranked[0].Total = %v, want a real score", d.Ranked[0].Total)
	}
	if len(d.Ranked[0].Components) == 0 {
		t.Error("Ranked[0] carries no per-dimension components; the total cannot be audited")
	}

	// Rejections are the other half of the explanation: the most common
	// question a shadow report raises is why a cheaper endpoint was not chosen.
	if len(d.Rejected) == 0 {
		t.Error("no rejections recorded")
	}
}

// The shadow saving is the number the mode exists to produce, and it lives in
// its own field so it can never be summed with money actually saved.
func TestShadowSavingIsSeparateFromRealSaving(t *testing.T) {
	d := shadowDecision(t)

	if d.CounterfactualCost <= 0 {
		t.Fatalf("CounterfactualCost = %s, want the counterfactual's price", d.CounterfactualCost)
	}
	if d.CounterfactualCost >= d.BaselineCost {
		t.Errorf("CounterfactualCost %s is not below BaselineCost %s; "+
			"the worked example's counterfactual is cheaper",
			d.CounterfactualCost, d.BaselineCost)
	}

	saving, ok := d.ShadowSaving()
	if !ok {
		t.Fatal("ShadowSaving reported unmeasured despite a baseline and a counterfactual")
	}
	if saving != d.BaselineCost-d.CounterfactualCost {
		t.Errorf("ShadowSaving = %s, want BaselineCost - CounterfactualCost", saving)
	}
	if saving <= 0 {
		t.Errorf("ShadowSaving = %s, want a positive figure", saving)
	}

	// The two must stay distinguishable: one is banked, the other is not.
	if d.EstimatedSaved == saving {
		t.Error("the actual saving and the shadow saving are indistinguishable")
	}
}

// Modes other than shadow record no counterfactual, so nothing can mistake a
// served decision for a hypothetical one.
func TestNonShadowModesRecordNoCounterfactual(t *testing.T) {
	for _, mode := range []domain.BaselineMode{domain.ModeStrict, domain.ModeOptimize} {
		t.Run(string(mode), func(t *testing.T) {
			in := docInput()
			in.Request.Baseline.Mode = mode

			d, err := Route(in)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if d.Counterfactual != "" {
				t.Errorf("Counterfactual = %s in %s mode", d.Counterfactual, mode)
			}
			if _, ok := d.ShadowSaving(); ok {
				t.Errorf("ShadowSaving reported a figure in %s mode", mode)
			}
		})
	}
}

// A route with no baseline cannot produce a shadow figure, and reporting one
// would invent a comparison. Unmeasured is not zero.
func TestShadowWithoutBaselineIsUnmeasured(t *testing.T) {
	in := docInput()
	in.Request.Baseline = domain.Baseline{Mode: domain.ModeShadow}

	d, err := Route(in)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.SavingMeasured {
		t.Error("SavingMeasured is true without a baseline")
	}
	if _, ok := d.ShadowSaving(); ok {
		t.Error("ShadowSaving reported a figure with no baseline to compare against")
	}
}

// --- Phase 4: failover candidates ---

// equivCatalog has two deployments of one model plus a second, cheaper model.
func equivCatalog() *domain.Catalog {
	mk := func(id, model, deployment string, in domain.Rate) *domain.ModelEndpoint {
		return &domain.ModelEndpoint{
			ID: id, Provider: "openai", Model: model, Deployment: deployment,
			CredentialRef: "primary",
			Capabilities:  domain.Capabilities{Tools: true, Streaming: true},
			Limits:        domain.Limits{ContextWindow: 128000},
			Pricing:       domain.Pricing{Input: in, Output: in},
			Quality:       map[string]float64{"coding": 0.8},
		}
	}
	return &domain.Catalog{
		Version: "v1",
		Endpoints: map[string]*domain.ModelEndpoint{
			"pinned@us": mk("pinned@us", "pinned-model", "us", 10),
			"pinned@eu": mk("pinned@eu", "pinned-model", "eu", 10),
			"other@us":  mk("other@us", "other-model", "us", 1),
		},
		Routes: map[string]*domain.Route{
			"r": {
				Name:       "r",
				Candidates: []string{"pinned@us", "pinned@eu", "other@us"},
				BaselineID: "pinned@us",
				Weights:    map[string]float64{"cost": 1.0},
			},
		},
	}
}

func routeIn(mode domain.BaselineMode) (*domain.Decision, error) {
	return Route(Input{
		Request: &domain.Request{
			RouteName: "r",
			Baseline:  domain.Baseline{EndpointID: "pinned@us", Mode: mode},
			Estimate:  domain.Estimate{InputTokens: 100, MaxOutputTokens: 100},
		},
		Catalog: equivCatalog(),
		Policy:  &domain.Policy{Version: "t"},
	})
}

func TestStrictFailsOverOnlyToTheSameModel(t *testing.T) {
	d, err := routeIn(domain.ModeStrict)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	// The promise a strict tenant bought is "you get the model you named". They
	// did not buy "if us-east is down, your request fails" — so another
	// deployment of the same model is a legitimate recovery, and a cheaper
	// different model is the substitution they refused.
	if len(d.Failover) != 1 || d.Failover[0] != "pinned@eu" {
		t.Errorf("Failover = %v, want only the other deployment of the same model", d.Failover)
	}
	for _, id := range d.Failover {
		if id == "other@us" {
			t.Error("strict mode would fail over to a different model, which is the " +
				"substitution the tenant declined")
		}
	}
}

func TestShadowInheritsStrictFailoverRules(t *testing.T) {
	d, err := routeIn(domain.ModeShadow)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Counterfactual != "other@us" {
		t.Fatalf("counterfactual = %q, want the cheaper model", d.Counterfactual)
	}
	// Shadow serves the baseline and only *prices* the alternative. Falling over
	// into the ranking would serve the counterfactual for real — performing the
	// substitution the whole mode exists to merely measure.
	for _, id := range d.Failover {
		if id == d.Counterfactual {
			t.Error("shadow mode would fail over to its own counterfactual")
		}
	}
}

func TestOptimizeFailsOverAcrossTheRanking(t *testing.T) {
	d, err := routeIn(domain.ModeOptimize)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(d.Failover) == 0 {
		t.Fatal("no failover candidates in optimize mode")
	}
	// Permission to substitute was already granted, so the ranking is the
	// failover order and nothing is off limits.
	if len(d.Failover) != len(d.Ranked)-1 {
		t.Errorf("Failover = %v against a ranking of %d", d.Failover, len(d.Ranked))
	}
}

func TestOpenBreakersAreNotOfferedAsFailover(t *testing.T) {
	d, err := Route(Input{
		Request: &domain.Request{
			RouteName: "r",
			Baseline:  domain.Baseline{EndpointID: "pinned@us", Mode: domain.ModeStrict},
		},
		Catalog: equivCatalog(),
		Policy:  &domain.Policy{Version: "t"},
		Health: &domain.Health{Endpoints: map[string]domain.EndpointHealth{
			"pinned@eu": {CircuitOpen: true},
		}},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	// Listing it would spend an attempt discovering what the breaker already
	// established, inside a request that is already recovering from a failure.
	if len(d.Failover) != 0 {
		t.Errorf("Failover = %v, want the open endpoint excluded", d.Failover)
	}
}

func TestPinnedModelWithNoEquivalentHasNoFailover(t *testing.T) {
	cat := equivCatalog()
	delete(cat.Endpoints, "pinned@eu")
	cat.Routes["r"].Candidates = []string{"pinned@us", "other@us"}

	d, err := Route(Input{
		Request: &domain.Request{
			RouteName: "r",
			Baseline:  domain.Baseline{EndpointID: "pinned@us", Mode: domain.ModeStrict},
		},
		Catalog: cat,
		Policy:  &domain.Policy{Version: "t"},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	// Empty is the correct answer, not a gap to be filled with the nearest
	// alternative. The request succeeds on the endpoint named or it fails.
	if len(d.Failover) != 0 {
		t.Errorf("Failover = %v, want none", d.Failover)
	}
}

// --- Phase 5: observed quality closes the loop ---

// penaltyHealth is a health snapshot carrying one endpoint's observed quality
// revision.
func penaltyHealth(endpoint string, penalty float64) *domain.Health {
	return &domain.Health{Endpoints: map[string]domain.EndpointHealth{
		endpoint: {QualityPenalty: penalty},
	}}
}

func qualityRoute(floor float64, health *domain.Health) (*domain.Decision, error) {
	pol := &domain.Policy{Version: "t"}
	if floor > 0 {
		pol.QualityFloor = map[string]float64{"coding": floor}
	}
	cat := equivCatalog()
	// A route that ranks on quality, so the floor applies and the score moves.
	cat.Routes["r"].Weights = map[string]float64{"quality.coding": 0.5, "cost": 0.5}

	return Route(Input{
		Request: &domain.Request{
			RouteName: "r",
			Baseline:  domain.Baseline{EndpointID: "pinned@us", Mode: domain.ModeOptimize},
			Estimate:  domain.Estimate{InputTokens: 100, MaxOutputTokens: 100},
		},
		Catalog: cat,
		Policy:  pol,
		Health:  health,
	})
}

func TestObservedQualityCanFailTheFloor(t *testing.T) {
	// Both endpoints are asserted at 0.80 in the catalog. With a floor of 0.75
	// both pass, and the cheaper one wins.
	d, err := qualityRoute(0.75, nil)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Chosen != "other@us" {
		t.Fatalf("chosen = %q, want the cheaper endpoint", d.Chosen)
	}

	// Now it has been escalating: a tenth of its answers failed a validity
	// check, so its effective quality is 0.70 and the floor eliminates it. This
	// is ADR-0009's loop closing — the catalog was optimistic and routing
	// corrected itself without anyone editing a file.
	d, err = qualityRoute(0.75, penaltyHealth("other@us", 0.10))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if d.Chosen == "other@us" {
		t.Error("an endpoint whose observed quality is below the floor was still chosen")
	}

	rej, ok := rejectionFor(d, "other@us")
	if !ok || rej.Reason != domain.RejectBelowQualityFloor {
		t.Fatalf("rejection = %+v, want BelowQualityFloor", rej)
	}
	// The reported number is the one that actually decided. An operator reading
	// the asserted 0.80 against a floor of 0.75 would conclude the filter was
	// broken.
	if !strings.Contains(rej.Detail, "0.70") || !strings.Contains(rej.Detail, "penalty") {
		t.Errorf("detail = %q, want the effective score and the penalty", rej.Detail)
	}
}

func TestObservedQualityLowersTheScore(t *testing.T) {
	// Below the floor is elimination; above it, the penalty still has to move
	// the ranking. Otherwise an endpoint that escalates constantly keeps winning
	// as long as it clears the floor by a hair.
	clean, err := qualityRoute(0, nil)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	before, _ := scoreOf(clean, "other@us")

	penalised, err := qualityRoute(0, penaltyHealth("other@us", 0.2))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	after, _ := scoreOf(penalised, "other@us")

	if after >= before {
		t.Errorf("score went from %v to %v under a quality penalty", before, after)
	}
}

func TestNoPenaltyLeavesRoutingUnchanged(t *testing.T) {
	// The common path. Every endpoint with a clean record must route exactly as
	// it did before the feedback loop existed, or Phase 5 silently re-ranked
	// every route in production.
	withNil, err := qualityRoute(0.5, nil)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	withZero, err := qualityRoute(0.5, penaltyHealth("other@us", 0))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	if withNil.Chosen != withZero.Chosen {
		t.Errorf("chosen differs: %q vs %q", withNil.Chosen, withZero.Chosen)
	}
	a, _ := scoreOf(withNil, "other@us")
	b, _ := scoreOf(withZero, "other@us")
	if a != b {
		t.Errorf("score differs: %v vs %v", a, b)
	}
}
