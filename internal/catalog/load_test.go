package catalog

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/routing"
)

// asOf pins the clock so freshness tests assert behaviour rather than slowly
// rotting as the calendar advances past the fixture's verified_on dates.
var asOf = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func testOptions() Options {
	return Options{MaxPriceAge: 90 * 24 * time.Hour, Now: asOf}
}

// shippedOptions loads config/catalog.yaml as of the wall clock, not the frozen
// asOf above.
//
// The inline fixtures pin a clock so their attestations mean something fixed.
// The shipped file cannot share it: its attestations are refreshed whenever a
// human re-checks the providers' pricing pages, and the moment one is dated
// after asOf the frozen clock rejects it as "in the future" — a green file
// failing for being too current.
//
// MaxPriceAge is effectively disabled here on purpose. Whether the shipped
// prices have gone stale is a real question, but it belongs to the scheduled
// freshness job (.github/workflows/catalog-freshness.yml), which runs weekly at
// a 14-day lead and is allowed to go red on its own. Enforcing it here would
// instead fail every unrelated pull request the day an attestation aged out.
func shippedOptions() Options {
	return Options{MaxPriceAge: 100 * 365 * 24 * time.Hour, Now: time.Now()}
}

func load(t *testing.T, yaml string) (*domain.Catalog, error) {
	t.Helper()
	return Load(strings.NewReader(yaml), testOptions())
}

func mustLoad(t *testing.T, yaml string) *domain.Catalog {
	t.Helper()
	cat, err := load(t, yaml)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	return cat
}

// minimal is the smallest catalog that loads, used as a base for mutation.
const minimal = `
version: "v1"
endpoints:
  - id: p/a@r
    provider: p
    model: a
    deployment: r
    credential_ref: c
    capabilities: {streaming: true, tools: true}
    limits: {context_window: 100000, max_output_tokens: 4096}
    pricing: {input: 1.00, output: 2.00, verified_on: 2026-07-15}
    quality: {coding: 0.8}
routes:
  - name: relay/r
    candidates: [p/a@r]
    baseline: p/a@r
    weights: {cost: 1.0}
`

// TestLoadShippedCatalog keeps config/catalog.yaml honest. A shipped example
// that no test loads is an example that silently stops being valid.
func TestLoadShippedCatalog(t *testing.T) {
	cat, err := LoadFile(filepath.Join("..", "..", "config", "catalog.yaml"), shippedOptions())
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if cat.Version != "2026-08-04.1" {
		t.Errorf("version = %q", cat.Version)
	}
	if len(cat.Endpoints) != 5 {
		t.Errorf("loaded %d endpoints, want 5", len(cat.Endpoints))
	}
	if len(cat.Routes) != 3 {
		t.Errorf("loaded %d routes, want 3", len(cat.Routes))
	}

	// The zero-key demo route is what lets someone watch substitution work
	// without buying anything first, so its shape is asserted rather than
	// assumed: a free candidate, an expensive baseline, and no requirement the
	// free candidate cannot meet.
	t.Run("the zero-key demo route stays servable without a credential", func(t *testing.T) {
		rt, ok := cat.Route("relay/zero-key-demo")
		if !ok {
			t.Fatal("relay/zero-key-demo is missing; the keyless demo is gone")
		}
		if len(rt.Require) != 0 {
			t.Errorf("route declares %d requirements; the local endpoint has no "+
				"capabilities beyond streaming and cannot clear them", len(rt.Require))
		}
		local, ok := cat.Endpoint("ollama/qwen-coder@local")
		if !ok {
			t.Fatal("ollama/qwen-coder@local is missing")
		}
		if local.Pricing.Input != 0 || local.Pricing.Output != 0 {
			t.Errorf("the local endpoint is priced %d/%d; the demo depends on it being free",
				local.Pricing.Input, local.Pricing.Output)
		}
		var found bool
		for _, id := range rt.Candidates {
			found = found || id == local.ID
		}
		if !found {
			t.Error("the free endpoint is not a candidate on its own demo route")
		}
	})

	t.Run("dollars become integer micro-dollars", func(t *testing.T) {
		sonnet, ok := cat.Endpoint("anthropic/claude-sonnet-5@us-east")
		if !ok {
			t.Fatal("sonnet missing")
		}
		if got := sonnet.Pricing.Input; got != 2_000_000 {
			t.Errorf("input rate = %d, want 2000000", got)
		}
		if got := sonnet.Pricing.Output; got != 10_000_000 {
			t.Errorf("output rate = %d, want 10000000", got)
		}
		if got := sonnet.Pricing.CachedInput; got != 200_000 {
			t.Errorf("cached input rate = %d, want 200000", got)
		}
	})

	t.Run("prices that are not binary-representable round correctly", func(t *testing.T) {
		// 0.15 * 1e6 is 150000.00000000003 in float64, and 0.075 * 1e6 is
		// 74999.99999999999. Truncation would lose a micro-dollar on each.
		mini, _ := cat.Endpoint("openai/gpt-4o-mini@us-east")
		if got := mini.Pricing.Input; got != 150_000 {
			t.Errorf("0.15/1M = %d, want 150000", got)
		}
		if got := mini.Pricing.CachedInput; got != 75_000 {
			t.Errorf("0.075/1M = %d, want 75000", got)
		}
	})

	t.Run("aliases resolve", func(t *testing.T) {
		ep, ok := cat.Resolve("claude-sonnet-5")
		if !ok || ep.ID != "anthropic/claude-sonnet-5@us-east" {
			t.Errorf("Resolve(alias) = %v, %v", ep, ok)
		}
	})

	t.Run("structured constraints convert", func(t *testing.T) {
		rt, _ := cat.Route("relay/fast-coder")
		want := []domain.Constraint{
			{Capability: "tools"},
			{QualityDim: "coding", QualityMin: 0.60},
		}
		if len(rt.Require) != len(want) {
			t.Fatalf("require = %+v, want %+v", rt.Require, want)
		}
		for i := range want {
			if rt.Require[i] != want[i] {
				t.Errorf("require[%d] = %+v, want %+v", i, rt.Require[i], want[i])
			}
		}
	})

	t.Run("free endpoints need no price attestation", func(t *testing.T) {
		qwen, ok := cat.Endpoint("ollama/qwen-coder@local")
		if !ok {
			t.Fatal("qwen missing")
		}
		if qwen.Pricing.Input != 0 || qwen.Pricing.Output != 0 {
			t.Error("free endpoint has non-zero pricing")
		}
	})
}

// TestShippedCatalogRoutes wires the loader to the router, which is the only
// way to know the file describes a catalog that can actually serve a request.
func TestShippedCatalogRoutes(t *testing.T) {
	cat, err := LoadFile(filepath.Join("..", "..", "config", "catalog.yaml"), shippedOptions())
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	d, err := routing.Route(routing.Input{
		Catalog: cat,
		Policy:  &domain.Policy{Version: "t", OptimizationMode: domain.ModeOptimize},
		Request: &domain.Request{
			RouteName: "relay/fast-coder",
			Baseline: domain.Baseline{
				EndpointID: "anthropic/claude-opus-5@us-east",
				Mode:       domain.ModeOptimize,
			},
			Need:     domain.Need{Tools: true},
			Estimate: domain.Estimate{InputTokens: 8000, MaxOutputTokens: 1500, ExpectedOutputTokens: 1500},
		},
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	if d.Chosen == "" {
		t.Fatal("no endpoint chosen")
	}
	if !d.SavingMeasured {
		t.Error("SavingMeasured = false; the route declares a baseline")
	}
	// 8000 input x $5/MTok + 1500 output x $25/MTok = $0.040 + $0.0375.
	// Spelled out because this number is the denominator of every saving the
	// product reports: if the shipped price for the baseline changes, this is
	// the assertion that should force somebody to look at it deliberately.
	if d.BaselineCost != 77_500 {
		t.Errorf("BaselineCost = %s, want $0.077500", d.BaselineCost)
	}
	// qwen-coder lacks tool support and must not survive the filter.
	for _, c := range d.Ranked {
		if c.EndpointID == "ollama/qwen-coder@local" {
			t.Error("a tool-less endpoint was ranked for a tools request")
		}
	}
}

func TestStrictDecoding(t *testing.T) {
	// The reason strict decoding exists: this typo would otherwise parse
	// cleanly, leave Capabilities zero, and quietly remove the endpoint from
	// every tools request without a single log line.
	yaml := strings.Replace(minimal, "capabilities:", "capabilties:", 1)

	_, err := load(t, yaml)
	if err == nil {
		t.Fatal("a misspelled field loaded successfully")
	}
	if !strings.Contains(err.Error(), "capabilties") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	yaml := `
version: ""
endpoints:
  - id: p/a@r
    provider: ""
    model: ""
    limits: {context_window: 0}
    pricing: {input: 5.00}
    quality: {coding: 1.7}
`
	_, err := load(t, yaml)
	if err == nil {
		t.Fatal("expected an error")
	}

	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("error is %T, want *LoadError", err)
	}

	// version, provider, model, context_window, quality range, verified_on.
	if len(le.Problems) < 6 {
		t.Errorf("reported %d problems, want at least 6 in one pass:\n%v", len(le.Problems), err)
	}
	for _, want := range []string{"version", "provider", "model", "context_window", "verified_on"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestPricingAttestation(t *testing.T) {
	tests := []struct {
		name    string
		pricing string
		opts    Options
		wantErr string
	}{
		{
			name:    "fresh price loads",
			pricing: "{input: 1.00, output: 2.00, verified_on: 2026-07-15}",
		},
		{
			name:    "free endpoint needs no attestation",
			pricing: "{input: 0.00, output: 0.00}",
		},
		{
			name:    "priced endpoint without attestation is rejected",
			pricing: "{input: 1.00, output: 2.00}",
			wantErr: "verified_on is required",
		},
		{
			name:    "stale price is rejected",
			pricing: "{input: 1.00, output: 2.00, verified_on: 2026-01-01}",
			wantErr: "older than",
		},
		{
			name:    "stale price passes when the check is disabled",
			pricing: "{input: 1.00, output: 2.00, verified_on: 2020-01-01}",
			opts:    Options{Now: asOf},
		},
		{
			name:    "malformed date",
			pricing: "{input: 1.00, output: 2.00, verified_on: 'last tuesday'}",
			wantErr: "not a YYYY-MM-DD date",
		},
		{
			// A future date is how a stale-price check gets defeated by
			// accident: it silences the alarm without re-verifying anything.
			name:    "future date is rejected",
			pricing: "{input: 1.00, output: 2.00, verified_on: 2027-01-01}",
			wantErr: "in the future",
		},
		{
			name:    "negative price",
			pricing: "{input: -1.00, output: 2.00, verified_on: 2026-07-15}",
			wantErr: "negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			yaml := strings.Replace(minimal,
				"pricing: {input: 1.00, output: 2.00, verified_on: 2026-07-15}",
				"pricing: "+tc.pricing, 1)

			opts := tc.opts
			if opts == (Options{}) {
				opts = testOptions()
			}
			_, err := Load(strings.NewReader(yaml), opts)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestRejectsAmbiguityAndDuplication(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			// A duplicate would otherwise overwrite the first entry silently,
			// taking its prices with it.
			name: "duplicate endpoint id",
			yaml: `
version: "v1"
endpoints:
  - id: p/a@r
    provider: p
    model: a
    limits: {context_window: 100000}
    pricing: {input: 1.00, output: 2.00, verified_on: 2026-07-15}
  - id: p/a@r
    provider: p
    model: a-again
    limits: {context_window: 1000}
    pricing: {input: 99.00, output: 99.00, verified_on: 2026-07-15}
`,
			wantErr: "duplicate endpoint id",
		},
		{
			name:    "alias shadowing an endpoint id",
			yaml:    minimal + "aliases: {p/a@r: p/a@r}\n",
			wantErr: "shadows the endpoint",
		},
		{
			name:    "alias pointing nowhere",
			yaml:    minimal + "aliases: {ghost: p/missing@r}\n",
			wantErr: "unknown endpoint",
		},
		{
			name: "duplicate candidate in a route",
			yaml: strings.Replace(minimal,
				"candidates: [p/a@r]", "candidates: [p/a@r, p/a@r]", 1),
			wantErr: "listed more than once",
		},
		{
			name: "unknown candidate",
			yaml: strings.Replace(minimal,
				"candidates: [p/a@r]", "candidates: [p/missing@r]", 1),
			wantErr: "unknown candidate",
		},
		{
			name: "weights that do not sum to one",
			yaml: strings.Replace(minimal,
				"weights: {cost: 1.0}", "weights: {cost: 0.5, latency: 0.9}", 1),
			wantErr: "must sum to 1",
		},
		{
			name: "unknown scoring dimension",
			yaml: strings.Replace(minimal,
				"weights: {cost: 1.0}", "weights: {vibes: 1.0}", 1),
			wantErr: "unknown scoring dimension",
		},
		{
			name: "unknown capability in a constraint",
			yaml: strings.Replace(minimal,
				"weights: {cost: 1.0}",
				"weights: {cost: 1.0}\n    require:\n      - capability: telepathy", 1),
			wantErr: "capability \"telepathy\" is not one of",
		},
		{
			name: "constraint with neither capability nor quality",
			yaml: strings.Replace(minimal,
				"weights: {cost: 1.0}",
				"weights: {cost: 1.0}\n    require:\n      - min: 0.5", 1),
			wantErr: "requires either capability or quality",
		},
		{
			name: "quality constraint out of range",
			yaml: strings.Replace(minimal,
				"weights: {cost: 1.0}",
				"weights: {cost: 1.0}\n    require:\n      - quality: coding\n        min: 1.5", 1),
			wantErr: "must be between 0 and 1",
		},
		{
			name: "invalid lifecycle status",
			yaml: `
version: "v1"
endpoints:
  - id: p/a@r
    provider: p
    model: a
    limits: {context_window: 100000}
    pricing: {input: 1.00, output: 2.00, verified_on: 2026-07-15}
    lifecycle: {status: mostly-fine}
`,
			wantErr: "is not one of ga, preview",
		},
		{
			name:    "empty file",
			yaml:    "",
			wantErr: "file is empty",
		},
		{
			name:    "no endpoints",
			yaml:    "version: \"v1\"\n",
			wantErr: "at least one endpoint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.yaml)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v,\nwant it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	cat := mustLoad(t, minimal)

	ep, _ := cat.Endpoint("p/a@r")
	if ep.Lifecycle.Status != domain.StatusGA {
		t.Errorf("lifecycle.status = %q, want ga by default", ep.Lifecycle.Status)
	}

	rt, _ := cat.Route("relay/r")
	if rt.MaxAttempts != 1 {
		t.Errorf("max_attempts = %d, want 1 by default", rt.MaxAttempts)
	}
}

func TestLoadedMapsAreNotSharedWithTheDecoder(t *testing.T) {
	// The catalog is handed to the router as an immutable snapshot. Mutating a
	// returned map must not reach back into anything the loader still holds.
	cat := mustLoad(t, minimal)
	ep, _ := cat.Endpoint("p/a@r")
	ep.Quality["coding"] = 0.1

	again := mustLoad(t, minimal)
	epAgain, _ := again.Endpoint("p/a@r")
	if epAgain.Quality["coding"] != 0.8 {
		t.Errorf("quality leaked across loads: %v", epAgain.Quality["coding"])
	}
}

func TestLoadFileErrors(t *testing.T) {
	if _, err := LoadFile(filepath.Join("testdata", "does-not-exist.yaml"), testOptions()); err == nil {
		t.Error("loading a missing file succeeded")
	}
}

func TestLoadErrorFormatting(t *testing.T) {
	single := &LoadError{Problems: []Problem{{Path: "version", Msg: "is required"}}}
	if got := single.Error(); got != "catalog: version: is required" {
		t.Errorf("single-problem error = %q", got)
	}

	multi := &LoadError{Problems: []Problem{
		{Path: "version", Msg: "is required"},
		{Path: "endpoints[0]", Msg: "id is required"},
	}}
	got := multi.Error()
	if !strings.Contains(got, "2 problems") ||
		!strings.Contains(got, "version") ||
		!strings.Contains(got, "endpoints[0]") {
		t.Errorf("multi-problem error = %q", got)
	}
}

// --- Phase 3: route response cache ---

func TestRouteCacheDefaults(t *testing.T) {
	cat := mustLoad(t, withRouteCache("cache:\n      enabled: true"))

	rc := cat.Routes["r"].Cache
	if !rc.Enabled {
		t.Fatal("cache was not enabled")
	}
	// Zero is not unbounded. A cache with no expiry serves last month's answer
	// to this month's question with no way to notice.
	if got := rc.EffectiveTTL(); got != domain.DefaultCacheTTL {
		t.Errorf("EffectiveTTL = %v, want %v", got, domain.DefaultCacheTTL)
	}
	if rc.AllowTemperature {
		t.Error("allow_temperature defaulted to true; a caller who asked for variation " +
			"must not silently get one answer forever")
	}
}

func TestRouteCacheRejections(t *testing.T) {
	tests := map[string]struct {
		block string
		want  string
	}{
		// An operator who wrote a TTL and no enabled:true believes caching is on.
		// Accepting the file means they find out from a savings report months later.
		"configured but off": {"cache:\n      ttl: 5m", "not enabled"},
		"unparseable ttl":    {"cache:\n      enabled: true\n      ttl: 3600", "is not a duration"},
		"negative ttl":       {"cache:\n      enabled: true\n      ttl: -5m", "must be positive"},
		"absurd ttl":         {"cache:\n      enabled: true\n      ttl: 72h", "exceeds the"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, withRouteCache(tc.block))
			if err == nil {
				t.Fatalf("loaded a catalog with %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestUnknownCacheFieldIsRejected(t *testing.T) {
	// Strict decoding, for the same reason the rest of the file has it: a typo
	// in a cache setting that is silently ignored produces a cache whose
	// behaviour is not what anybody believes it to be.
	_, err := load(t, withRouteCache("cache:\n      enabled: true\n      tt1: 5m"))
	if err == nil {
		t.Fatal("a misspelled cache field was accepted")
	}
}

func withRouteCache(block string) string {
	return `
version: "v1"
endpoints:
  - id: e1
    provider: openai
    model: m
    limits: {context_window: 1000}
routes:
  - name: r
    candidates: [e1]
    weights: {cost: 1.0}
    ` + block + `
`
}
