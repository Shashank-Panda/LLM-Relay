package gateway

import (
	"errors"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
)

func testCatalog() *domain.Catalog {
	return &domain.Catalog{
		Version: "v1",
		Endpoints: map[string]*domain.ModelEndpoint{
			"openai/cheap@us": {ID: "openai/cheap@us", Provider: "openai"},
			"openai/dear@us":  {ID: "openai/dear@us", Provider: "openai"},
		},
		Aliases: map[string]string{"cheap": "openai/cheap@us"},
		Routes: map[string]*domain.Route{
			"relay/test": {
				Name:       "relay/test",
				Candidates: []string{"openai/cheap@us", "openai/dear@us"},
				BaselineID: "openai/dear@us",
				Weights:    map[string]float64{domain.DimCost: 1},
			},
			"relay/unmeasured": {
				Name:       "relay/unmeasured",
				Candidates: []string{"openai/cheap@us"},
				Weights:    map[string]float64{domain.DimCost: 1},
			},
		},
	}
}

func TestResolveBaseline(t *testing.T) {
	cat := testCatalog()

	t.Run("a pinned endpoint is its own baseline", func(t *testing.T) {
		_, b, err := ResolveBaseline(cat, "openai/cheap@us", domain.ModeStrict)
		if err != nil {
			t.Fatalf("ResolveBaseline: %v", err)
		}
		if b.EndpointID != "openai/cheap@us" || b.Source != SourceExplicitModel {
			t.Errorf("baseline = %+v", b)
		}
	})

	t.Run("an alias resolves to its target", func(t *testing.T) {
		_, b, err := ResolveBaseline(cat, "cheap", domain.ModeStrict)
		if err != nil {
			t.Fatalf("ResolveBaseline: %v", err)
		}
		if b.EndpointID != "openai/cheap@us" {
			t.Errorf("EndpointID = %q, want the alias target", b.EndpointID)
		}
	})

	t.Run("a route uses its declared baseline", func(t *testing.T) {
		route, b, err := ResolveBaseline(cat, "relay/test", domain.ModeOptimize)
		if err != nil {
			t.Fatalf("ResolveBaseline: %v", err)
		}
		if route != "relay/test" {
			t.Errorf("route = %q", route)
		}
		if b.EndpointID != "openai/dear@us" || b.Source != SourceRouteDefault {
			t.Errorf("baseline = %+v", b)
		}
	})

	// A route with no baseline reports savings as unmeasured, which is a
	// different fact from a measured saving of zero. The two must never be
	// summed, so the distinction has to survive from here.
	t.Run("a route without a baseline reports no endpoint", func(t *testing.T) {
		_, b, err := ResolveBaseline(cat, "relay/unmeasured", domain.ModeOptimize)
		if err != nil {
			t.Fatalf("ResolveBaseline: %v", err)
		}
		if b.EndpointID != "" {
			t.Errorf("EndpointID = %q, want empty", b.EndpointID)
		}
	})

	// Relay does not guess which model a caller meant. A near-miss served from
	// something else would be a substitution nobody consented to.
	t.Run("an unknown model fails rather than guessing", func(t *testing.T) {
		_, _, err := ResolveBaseline(cat, "gpt-9", domain.ModeStrict)
		var unknown *ErrUnknownModel
		if !errors.As(err, &unknown) {
			t.Fatalf("err = %v, want *ErrUnknownModel", err)
		}
		if unknown.Model != "gpt-9" {
			t.Errorf("Model = %q", unknown.Model)
		}
	})
}

// A pinned model still needs a route, because a route is what names a candidate
// set and the weights to rank it by. The lookup must be deterministic: two
// identical requests cannot resolve to different routes.
func TestRouteForIsDeterministic(t *testing.T) {
	cat := testCatalog()
	cat.Routes["relay/aaa"] = &domain.Route{
		Name: "relay/aaa", Candidates: []string{"openai/dear@us"},
		BaselineID: "openai/dear@us", Weights: map[string]float64{domain.DimCost: 1},
	}

	first, _, err := ResolveBaseline(cat, "openai/dear@us", domain.ModeStrict)
	if err != nil {
		t.Fatalf("ResolveBaseline: %v", err)
	}
	for range 100 {
		got, _, _ := ResolveBaseline(cat, "openai/dear@us", domain.ModeStrict)
		if got != first {
			t.Fatalf("route resolved to %q then %q for identical input", first, got)
		}
	}
	if first != "relay/aaa" {
		t.Errorf("route = %q, want the lexicographically first match", first)
	}
}

func TestResolveMode(t *testing.T) {
	optimizing := &domain.Policy{OptimizationMode: domain.ModeOptimize}

	tests := []struct {
		name   string
		pin    string
		policy *domain.Policy
		want   domain.BaselineMode
	}{
		{
			// The escape hatch a customer needs on the one request where
			// substitution is unacceptable. An escape hatch a policy can
			// override is not one.
			name: "an explicit strict pin overrides an optimizing policy",
			pin:  "strict", policy: optimizing, want: domain.ModeStrict,
		},
		{
			name: "shadow can be requested per request",
			pin:  "shadow", policy: optimizing, want: domain.ModeShadow,
		},
		{
			// Permission to substitute is the tenant's to give, and a
			// per-request header is not where that consent lives.
			name: "asking for optimize does not grant it",
			pin:  "optimize", policy: domain.DefaultPolicy(), want: domain.ModeStrict,
		},
		{
			name: "no pin falls through to policy",
			pin:  "", policy: optimizing, want: domain.ModeOptimize,
		},
		{
			name: "an unrecognised pin is ignored, not an error",
			pin:  "fastest", policy: optimizing, want: domain.ModeOptimize,
		},
		{
			// The zero value is strict. Nobody gets substituted by forgetting
			// to configure something.
			name: "an empty policy is strict",
			pin:  "", policy: &domain.Policy{}, want: domain.ModeStrict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveMode(tc.pin, tc.policy); got != tc.want {
				t.Errorf("ResolveMode(%q) = %s, want %s", tc.pin, got, tc.want)
			}
		})
	}
}
