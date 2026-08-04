package domain

import (
	"strings"
	"testing"
)

func TestRateCost(t *testing.T) {
	tests := []struct {
		name   string
		rate   Rate
		tokens int
		want   Money
	}{
		{"opus input, 8k tokens", 15_000_000, 8_000, 120_000},
		{"opus output, 1.5k tokens", 75_000_000, 1_500, 112_500},
		{"sonnet input, 8k tokens", 3_000_000, 8_000, 24_000},
		{"gpt-4o-mini input, 8k tokens", 150_000, 8_000, 1_200},
		{"gpt-4o-mini output, 1.5k tokens", 600_000, 1_500, 900},
		{"free endpoint", 0, 100_000, 0},
		{"zero tokens", 15_000_000, 0, 0},
		{"negative tokens", 15_000_000, -5, 0},
		{"negative rate", -1, 100, 0},
		{
			// $1000 per million tokens over 2M tokens is $2,000. The number
			// that matters here is the intermediate product, 2e15 — three
			// orders of magnitude below the int64 ceiling of ~9.2e18.
			name: "no overflow at implausible scale", rate: 1_000_000_000,
			tokens: 2_000_000, want: 2_000_000_000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rate.Cost(tc.tokens); got != tc.want {
				t.Errorf("Cost(%d) = %d, want %d", tc.tokens, got, tc.want)
			}
		})
	}
}

func TestMoneyFormatting(t *testing.T) {
	if got := Money(232_500).String(); got != "$0.232500" {
		t.Errorf("String() = %q, want %q", got, "$0.232500")
	}
	if got := Money(232_500).Dollars(); got != 0.2325 {
		t.Errorf("Dollars() = %v, want 0.2325", got)
	}
}

func TestEndpointFits(t *testing.T) {
	ep := &ModelEndpoint{Limits: Limits{ContextWindow: 10_000}}

	tests := []struct {
		name string
		est  Estimate
		want bool
	}{
		{"fits exactly", Estimate{InputTokens: 8_000, MaxOutputTokens: 2_000}, true},
		{"one over", Estimate{InputTokens: 8_000, MaxOutputTokens: 2_001}, false},
		{
			// Fits on the expectation but not at the ceiling. A request that
			// fits on average and fails at the tail is a request that fails.
			name: "ceiling not expectation",
			est:  Estimate{InputTokens: 9_000, MaxOutputTokens: 4_000, ExpectedOutputTokens: 500},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ep.Fits(tc.est); got != tc.want {
				t.Errorf("Fits() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("unset context window fits nothing", func(t *testing.T) {
		if (&ModelEndpoint{}).Fits(Estimate{InputTokens: 1}) {
			t.Error("an endpoint with no declared window accepted a request")
		}
	})
}

func TestEndpointSupports(t *testing.T) {
	full := &ModelEndpoint{Capabilities: Capabilities{
		Streaming: true, Tools: true, JSONSchema: true, Vision: true, Reasoning: true,
	}}

	if missing, ok := full.Supports(Need{Tools: true, Vision: true}); !ok {
		t.Errorf("fully capable endpoint reported missing %q", missing)
	}

	// The zero value must be the least capable endpoint, not the most: a
	// half-filled catalog entry should fail the filter, not pass it.
	bare := &ModelEndpoint{}
	for _, tc := range []struct {
		need Need
		want string
	}{
		{Need{Tools: true}, "tools"},
		{Need{Vision: true}, "vision"},
		{Need{JSONSchema: true}, "json_schema"},
		{Need{Reasoning: true}, "reasoning"},
		{Need{Streaming: true}, "streaming"},
	} {
		missing, ok := bare.Supports(tc.need)
		if ok || missing != tc.want {
			t.Errorf("zero endpoint Supports(%+v) = (%q, %v), want (%q, false)",
				tc.need, missing, ok, tc.want)
		}
	}
}

func TestEndpointQualityFor(t *testing.T) {
	// An unscored dimension returns 0 so it fails any non-zero floor. Silence
	// in the catalog must not read as a passing grade.
	if got := (&ModelEndpoint{}).QualityFor("coding"); got != 0 {
		t.Errorf("QualityFor on nil map = %v, want 0", got)
	}
}

func TestRouteValidate(t *testing.T) {
	valid := func() *Route {
		return &Route{
			Name:       "relay/test",
			Candidates: []string{"a"},
			Weights:    map[string]float64{DimCost: 0.5, DimLatency: 0.5},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Route)
		wantErr string
	}{
		{"valid", func(*Route) {}, ""},
		{"no name", func(r *Route) { r.Name = "" }, "name is required"},
		{"no candidates", func(r *Route) { r.Candidates = nil }, "at least one candidate"},
		{"no weights", func(r *Route) { r.Weights = nil }, "weights are required"},
		{
			name:    "weights must sum to one",
			mutate:  func(r *Route) { r.Weights[DimCost] = 0.8 },
			wantErr: "must sum to 1",
		},
		{
			name:    "negative weight",
			mutate:  func(r *Route) { r.Weights[DimCost] = -0.5; r.Weights[DimLatency] = 1.5 },
			wantErr: "is negative",
		},
		{
			name:    "unknown dimension",
			mutate:  func(r *Route) { r.Weights = map[string]float64{"vibes": 1.0} },
			wantErr: "unknown scoring dimension",
		},
		{
			name:    "bare quality prefix is not a dimension",
			mutate:  func(r *Route) { r.Weights = map[string]float64{"quality.": 1.0} },
			wantErr: "unknown scoring dimension",
		},
		{
			name:    "quality dimension accepted",
			mutate:  func(r *Route) { r.Weights = map[string]float64{"quality.coding": 1.0} },
			wantErr: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(r)
			err := r.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func validCatalog() *Catalog {
	return &Catalog{
		Version: "v1",
		Endpoints: map[string]*ModelEndpoint{
			"p/a@r": {
				ID:      "p/a@r",
				Limits:  Limits{ContextWindow: 1000},
				Pricing: Pricing{Input: 1, Output: 2},
				Quality: map[string]float64{"coding": 0.5},
			},
		},
		Aliases: map[string]string{"a": "p/a@r"},
		Routes: map[string]*Route{
			"relay/r": {
				Name:       "relay/r",
				Candidates: []string{"p/a@r"},
				BaselineID: "p/a@r",
				Fallback:   "p/a@r",
				Weights:    map[string]float64{DimCost: 1.0},
			},
		},
	}
}

func TestCatalogValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Catalog)
		wantErr string
	}{
		{"valid", func(*Catalog) {}, ""},
		{"no endpoints", func(c *Catalog) { c.Endpoints = nil }, "no endpoints"},
		{
			name:    "key and ID disagree",
			mutate:  func(c *Catalog) { c.Endpoints["p/a@r"].ID = "other" },
			wantErr: "has ID",
		},
		{
			name:    "context window unset",
			mutate:  func(c *Catalog) { c.Endpoints["p/a@r"].Limits.ContextWindow = 0 },
			wantErr: "context_window must be positive",
		},
		{
			name:    "negative price",
			mutate:  func(c *Catalog) { c.Endpoints["p/a@r"].Pricing.Input = -1 },
			wantErr: "must not be negative",
		},
		{
			name:    "quality outside 0..1",
			mutate:  func(c *Catalog) { c.Endpoints["p/a@r"].Quality["coding"] = 1.5 },
			wantErr: "must be 0..1",
		},
		{
			name:    "dangling alias",
			mutate:  func(c *Catalog) { c.Aliases["ghost"] = "p/missing@r" },
			wantErr: "unknown endpoint",
		},
		{
			name:    "dangling candidate",
			mutate:  func(c *Catalog) { c.Routes["relay/r"].Candidates = []string{"p/missing@r"} },
			wantErr: "unknown candidate",
		},
		{
			name:    "dangling fallback",
			mutate:  func(c *Catalog) { c.Routes["relay/r"].Fallback = "p/missing@r" },
			wantErr: "unknown endpoint",
		},
		{
			name:    "dangling baseline",
			mutate:  func(c *Catalog) { c.Routes["relay/r"].BaselineID = "p/missing@r" },
			wantErr: "unknown endpoint",
		},
		{
			name:    "route key and name disagree",
			mutate:  func(c *Catalog) { c.Routes["relay/r"].Name = "other" },
			wantErr: "has name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validCatalog()
			tc.mutate(c)
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	t.Run("nil catalog", func(t *testing.T) {
		if err := (*Catalog)(nil).Validate(); err == nil {
			t.Error("nil catalog validated")
		}
	})
}

func TestCatalogResolve(t *testing.T) {
	c := validCatalog()

	if ep, ok := c.Resolve("a"); !ok || ep.ID != "p/a@r" {
		t.Errorf("Resolve(alias) = (%v, %v), want the aliased endpoint", ep, ok)
	}
	if ep, ok := c.Resolve("p/a@r"); !ok || ep.ID != "p/a@r" {
		t.Errorf("Resolve(id) = (%v, %v), want the endpoint", ep, ok)
	}
	if _, ok := c.Resolve("nope"); ok {
		t.Error("Resolve returned an unknown name")
	}
	if _, ok := (*Catalog)(nil).Resolve("a"); ok {
		t.Error("nil catalog resolved a name")
	}
}

func TestPolicyPermits(t *testing.T) {
	ep := &ModelEndpoint{ID: "anthropic/claude-sonnet-5@us-east", Deployment: "us-east"}

	tests := []struct {
		name   string
		policy *Policy
		want   bool
	}{
		{"nil policy permits", nil, true},
		{"empty policy permits", &Policy{}, true},
		{"exact allow", &Policy{Allow: []string{ep.ID}}, true},
		{"prefix allow", &Policy{Allow: []string{"anthropic/*"}}, true},
		{"star allow", &Policy{Allow: []string{"*"}}, true},
		{"allow excludes others", &Policy{Allow: []string{"openai/*"}}, false},
		{"exact deny", &Policy{Deny: []string{ep.ID}}, false},
		{"prefix deny", &Policy{Deny: []string{"anthropic/*"}}, false},
		{
			// Deny is evaluated first and is not overridable. A restriction
			// that an allow-list can cancel is not a restriction.
			name:   "deny beats allow",
			policy: &Policy{Allow: []string{"*"}, Deny: []string{"anthropic/*"}},
			want:   false,
		},
		{"matching region", &Policy{Regions: []string{"us-east"}}, true},
		{"non-matching region", &Policy{Regions: []string{"eu-west"}}, false},
		{"one of several regions", &Policy{Regions: []string{"eu-west", "us-east"}}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detail, got := tc.policy.Permits(ep)
			if got != tc.want {
				t.Errorf("Permits() = %v (%s), want %v", got, detail, tc.want)
			}
			if !got && detail == "" {
				t.Error("denial carries no detail")
			}
		})
	}
}

func TestPolicyMode(t *testing.T) {
	tests := []struct {
		name    string
		policy  *Policy
		request BaselineMode
		want    BaselineMode
	}{
		{"nil policy is strict", nil, "", ModeStrict},
		{"empty policy is strict", &Policy{}, "", ModeStrict},
		{"policy default applies", &Policy{OptimizationMode: ModeOptimize}, "", ModeOptimize},
		{
			// X-Relay-Pin: strict escaping a tenant-wide optimize setting.
			name:   "request overrides policy",
			policy: &Policy{OptimizationMode: ModeOptimize}, request: ModeStrict,
			want: ModeStrict,
		},
		{
			name:   "invalid request mode falls back to policy",
			policy: &Policy{OptimizationMode: ModeShadow}, request: "nonsense",
			want: ModeShadow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Mode(tc.request); got != tc.want {
				t.Errorf("Mode(%q) = %q, want %q", tc.request, got, tc.want)
			}
		})
	}
}

func TestHealthForIsNilSafe(t *testing.T) {
	// Routing must not fail closed on missing telemetry: an absent snapshot
	// means "assume available", per ADR-0010.
	var h *Health
	if got := h.For("anything"); got.CircuitOpen {
		t.Error("nil health reported an open circuit")
	}
	if got := (&Health{}).For("anything"); got.CircuitOpen {
		t.Error("empty health reported an open circuit")
	}
}

func TestSortedKeysIsStable(t *testing.T) {
	m := map[string]float64{"quality.coding": 1, "cost": 2, "latency": 3, "cache_affinity": 4}
	want := []string{"cache_affinity", "cost", "latency", "quality.coding"}
	for range 100 {
		got := SortedKeys(m)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("SortedKeys = %v, want %v", got, want)
			}
		}
	}
	if SortedKeys(map[string]int(nil)) != nil {
		t.Error("SortedKeys on an empty map should return nil")
	}
}
