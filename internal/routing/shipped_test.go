package routing_test

import (
	"path/filepath"
	"testing"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/routing"
	"github.com/Shashank-Panda/relay/internal/tenant"
)

func shippedCatalog(t *testing.T) *domain.Catalog {
	t.Helper()
	cat, err := catalog.LoadFile(
		filepath.Join("..", "..", "config", "catalog.yaml"),
		// The attestation window is deliberately not enforced here. This test is
		// about routing behaviour; the expiry has its own check in cmd/relay
		// -validate and its own scheduled CI job, and letting it fail this test
		// too would turn one calendar event into a dozen unrelated red builds.
		catalog.Options{MaxPriceAge: 0},
	)
	if err != nil {
		t.Fatalf("load shipped catalog: %v", err)
	}
	return cat
}

// TestZeroKeyDemoSubstitutesToTheFreeEndpoint is the assertion the whole
// "docker compose up and watch it work" story rests on.
//
// It is also the mistake that was nearly shipped. The free local endpoint is
// listed as a candidate on relay/fast-coder, which makes that route look like a
// ready-made keyless demo — but that route requires the tools capability and a
// coding score of at least 0.60, and the endpoint declares neither. It is
// rejected on both counts, so a keyless request there has no viable candidate
// at all. Nothing about the catalog says so on inspection; only routing does.
func TestZeroKeyDemoSubstitutesToTheFreeEndpoint(t *testing.T) {
	cat := shippedCatalog(t)

	reg, err := tenant.LoadFile(filepath.Join("..", "..", "config", "tenants.yaml"))
	if err != nil {
		t.Fatalf("load shipped tenants: %v", err)
	}
	tn, ok := reg.Resolve("sk-relay-demo-optimize-0f1e2d3c4b5a")
	if !ok {
		t.Fatal("the demo key does not resolve; the digest in tenants.yaml is wrong")
	}
	if tn.Policy.OptimizationMode != domain.ModeOptimize {
		t.Fatalf("demo tenant mode = %q, want optimize; without it substitution "+
			"cannot be demonstrated by any request against the shipped config",
			tn.Policy.OptimizationMode)
	}

	const (
		route = "relay/zero-key-demo"
		free  = "ollama/qwen-coder@local"
		opus  = "anthropic/claude-opus-5@us-east"
	)

	d, err := routing.Route(routing.Input{
		Request: &domain.Request{
			ID:        "req-demo",
			Tenant:    tn.ID,
			RouteName: route,
			Baseline: domain.Baseline{
				EndpointID: opus,
				Mode:       domain.ModeOptimize,
				Source:     "route_default",
			},
			Estimate: domain.Estimate{
				InputTokens:          400,
				MaxOutputTokens:      256,
				ExpectedOutputTokens: 256,
			},
		},
		Catalog: cat,
		Policy:  tn.Policy,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	if d.Chosen != free {
		t.Errorf("chose %q, want %q", d.Chosen, free)
		for _, r := range d.Rejected {
			if r.EndpointID == free {
				t.Errorf("the free endpoint was rejected: %s — %s", r.Reason, r.Detail)
			}
		}
	}
	if d.Baseline.EndpointID != opus {
		t.Errorf("baseline = %q, want %q", d.Baseline.EndpointID, opus)
	}
	if d.EstimatedSaved <= 0 {
		t.Errorf("estimated saving = %d, want positive: serving a free endpoint against "+
			"a frontier baseline is the entire point of this route", d.EstimatedSaved)
	}
	if !d.SavingMeasured {
		t.Error("saving is unmeasured; the demo would report nothing")
	}
}

// TestFastCoderRejectsTheFreeEndpoint records why the demo route had to exist,
// so nobody deletes it as redundant.
//
// If this ever starts failing because the local endpoint gained the tools
// capability and a higher asserted coding score, that is good news and the demo
// route can be reconsidered — but the score must come from cmd/relay-eval, not
// from editing the catalog to make a demo pass.
func TestFastCoderRejectsTheFreeEndpoint(t *testing.T) {
	cat := shippedCatalog(t)

	d, err := routing.Route(routing.Input{
		Request: &domain.Request{
			ID:        "req-fc",
			Tenant:    "demo",
			RouteName: "relay/fast-coder",
			Baseline: domain.Baseline{
				EndpointID: "anthropic/claude-opus-5@us-east",
				Mode:       domain.ModeOptimize,
			},
			Need:     domain.Need{Tools: true},
			Estimate: domain.Estimate{InputTokens: 400, MaxOutputTokens: 256, ExpectedOutputTokens: 256},
		},
		Catalog: cat,
		Policy:  &domain.Policy{OptimizationMode: domain.ModeOptimize},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	for _, c := range d.Ranked {
		if c.EndpointID == "ollama/qwen-coder@local" {
			t.Fatal("the free endpoint now survives relay/fast-coder's filters; " +
				"if that is intentional, relay/zero-key-demo may no longer be needed")
		}
	}

	var reason domain.RejectReason
	for _, r := range d.Rejected {
		if r.EndpointID == "ollama/qwen-coder@local" {
			reason = r.Reason
		}
	}
	if reason == "" {
		t.Fatal("the free endpoint is neither ranked nor rejected on relay/fast-coder")
	}
	t.Logf("relay/fast-coder rejects the free endpoint as %s — this is why "+
		"relay/zero-key-demo exists", reason)
}
