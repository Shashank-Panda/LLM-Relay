package optimize_test

import (
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/optimize"
	"github.com/Shashank-Panda/relay/internal/routing"
)

const (
	epSmallCheap = "provider/small-cheap@us"
	epLargeDear  = "provider/large-dear@us"
	routeName    = "relay/test"
)

func ceilingCatalog() *domain.Catalog {
	ep := func(id string, ctx int, in, out domain.Rate, coding float64) *domain.ModelEndpoint {
		return &domain.ModelEndpoint{
			ID: id, Provider: "provider", Model: id, Deployment: "us",
			CredentialRef: "c",
			Capabilities:  domain.Capabilities{Streaming: true, Tools: true},
			Limits:        domain.Limits{ContextWindow: ctx, MaxOutputTokens: 16384},
			Pricing:       domain.Pricing{Input: in, Output: out},
			Quality:       map[string]float64{"coding": coding},
			Lifecycle:     domain.Lifecycle{Status: domain.StatusGA},
		}
	}
	return &domain.Catalog{
		Version: "v1",
		Endpoints: map[string]*domain.ModelEndpoint{
			epSmallCheap: ep(epSmallCheap, 32_768, 100_000, 400_000, 0.70),
			epLargeDear:  ep(epLargeDear, 200_000, 3_000_000, 15_000_000, 0.90),
		},
		Routes: map[string]*domain.Route{
			routeName: {
				Name:       routeName,
				Candidates: []string{epSmallCheap, epLargeDear},
				BaselineID: epLargeDear,
				Weights:    map[string]float64{domain.DimCost: 1.0},
			},
		},
	}
}

func longRequest() *domain.NormalizedRequest {
	// ~30,000 input tokens, and no max_tokens — the shape of a real request
	// from a caller who never tuned the parameter because nobody does.
	return &domain.NormalizedRequest{
		ID:        "req-1",
		RouteName: routeName,
		Baseline: domain.Baseline{
			EndpointID: epLargeDear,
			Mode:       domain.ModeOptimize,
			Source:     "explicit_model",
		},
		Messages: []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.ContentPart{{Kind: domain.PartText, Text: filler30k()}},
		}},
	}
}

func filler30k() string {
	b := make([]byte, 30_000*4)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func route(t *testing.T, req *domain.NormalizedRequest) *domain.Decision {
	t.Helper()
	d, err := routing.Route(routing.Input{
		Request: req.RoutingView(),
		Catalog: ceilingCatalog(),
		Policy:  &domain.Policy{Version: "p", OptimizationMode: domain.ModeOptimize},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	return d
}

// TestOutputCeilingWidensTheCandidateSet demonstrates the claim made in
// applyOutputCeiling: the ceiling's saving is mostly indirect.
//
// Providers bill generated tokens, not the ceiling, so capping max_tokens does
// not make a short answer cheaper. What it does is stop a nominal ceiling from
// disqualifying every small-context endpoint on the context-window check —
// quietly forcing routing onto expensive large-context models for requests that
// produce a few hundred tokens.
func TestOutputCeilingWidensTheCandidateSet(t *testing.T) {
	cfg := domain.RecommendedLevers()
	stats := &optimize.Stats{OutputTokensP95: map[string]int{routeName: 400}}

	t.Run("without a ceiling the cheap endpoint is eliminated", func(t *testing.T) {
		noCeiling := cfg
		noCeiling.OutputCeiling = false

		req, _ := optimize.New(noCeiling).Apply(longRequest(), stats)
		d := route(t, req)

		var reason domain.RejectReason
		for _, r := range d.Rejected {
			if r.EndpointID == epSmallCheap {
				reason = r.Reason
			}
		}
		if reason != domain.RejectContextTooSmall {
			t.Fatalf("small endpoint rejection = %q, want ContextTooSmall", reason)
		}
		if d.Chosen != epLargeDear {
			t.Errorf("chosen = %s, want the expensive endpoint", d.Chosen)
		}
		if d.EstimatedSaved != 0 {
			t.Errorf("saved %s, want nothing", d.EstimatedSaved)
		}
	})

	t.Run("with a ceiling it survives and wins on cost", func(t *testing.T) {
		req, ops := optimize.New(cfg).Apply(longRequest(), stats)

		if len(ops) == 0 {
			t.Fatal("no optimizations applied")
		}
		if req.Estimate.MaxOutputTokens != 600 { // 400 p95 x 1.5
			t.Fatalf("MaxOutputTokens = %d, want 600", req.Estimate.MaxOutputTokens)
		}

		d := route(t, req)

		for _, r := range d.Rejected {
			if r.EndpointID == epSmallCheap {
				t.Fatalf("small endpoint still rejected: %s (%s)", r.Reason, r.Detail)
			}
		}
		if d.Chosen != epSmallCheap {
			t.Fatalf("chosen = %s, want %s", d.Chosen, epSmallCheap)
		}
		if d.EstimatedSaved <= 0 {
			t.Errorf("saved %s, want a positive saving", d.EstimatedSaved)
		}
		// ~30k input at $3/1M vs $0.10/1M is the bulk of it.
		if d.EstimatedSaved < 80_000 {
			t.Errorf("saved only %s; expected roughly $0.09 on input alone", d.EstimatedSaved)
		}
	})
}

// TestOptimizerAndRouterAgreeOnNeeds guards the seam between the two: an
// optimizer-applied reasoning effort must not become a capability requirement,
// or enabling the lever would eliminate every non-reasoning endpoint.
func TestOptimizerAndRouterAgreeOnNeeds(t *testing.T) {
	cfg := domain.RecommendedLevers()
	cfg.EffortDownshift = true
	cfg.DefaultEffort = domain.EffortLow

	req, _ := optimize.New(cfg).Apply(longRequest(), &optimize.Stats{
		OutputTokensP95: map[string]int{routeName: 400},
	})

	if req.Params.ReasoningEffort == nil {
		t.Fatal("effort was not applied")
	}

	d := route(t, req)

	// Neither endpoint declares reasoning support. If the defaulted effort
	// counted as a requirement, both would be rejected and the route would fall
	// back to the baseline.
	for _, r := range d.Rejected {
		if r.Reason == domain.RejectMissingCapability {
			t.Fatalf("%s rejected for a capability the caller never asked for: %s",
				r.EndpointID, r.Detail)
		}
	}
	if d.Chosen != epSmallCheap {
		t.Errorf("chosen = %s, want %s", d.Chosen, epSmallCheap)
	}
}
