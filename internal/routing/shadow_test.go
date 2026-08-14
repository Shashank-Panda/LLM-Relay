package routing

import (
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
