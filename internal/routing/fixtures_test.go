package routing

import (
	"math"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// The fixtures below mirror the worked example in docs/routing.md §7 exactly.
// If the documentation and these numbers ever disagree, one of them is wrong
// and TestWorkedExample_MatchesDocumentation is what says so.

const (
	epOpus   = "anthropic/claude-opus-5@us-east"
	epSonnet = "anthropic/claude-sonnet-5@us-east"
	epMini   = "openai/gpt-4o-mini@us-east"
	epFlash  = "google/gemini-flash@us-central"
	epQwen   = "ollama/qwen-coder@local"

	routeFastCoder = "relay/fast-coder"
)

// usdPerMillion converts a documented per-million-token price to a Rate.
// Rounded rather than truncated: 0.15*1e6 is 150000.00000000003 in float, and
// truncation would silently shave a micro-dollar off every price ending in 5.
func usdPerMillion(usd float64) domain.Rate {
	return domain.Rate(math.Round(usd * 1_000_000))
}

type endpointSpec struct {
	id            string
	provider      domain.ProviderID
	contextWindow int
	tools         bool
	inUSD, outUSD float64
	coding        float64
	latency       time.Duration
}

func (s endpointSpec) build() *domain.ModelEndpoint {
	return &domain.ModelEndpoint{
		ID:            s.id,
		Provider:      s.provider,
		Model:         s.id,
		Deployment:    "us-east",
		CredentialRef: "primary",
		Capabilities: domain.Capabilities{
			Streaming:  true,
			Tools:      s.tools,
			JSONSchema: true,
		},
		Limits: domain.Limits{
			ContextWindow:   s.contextWindow,
			MaxOutputTokens: 64000,
		},
		Pricing: domain.Pricing{
			Input:  usdPerMillion(s.inUSD),
			Output: usdPerMillion(s.outUSD),
		},
		Quality:   map[string]float64{"coding": s.coding},
		Lifecycle: domain.Lifecycle{Status: domain.StatusGA},
	}
}

var docSpecs = []endpointSpec{
	{epOpus, "anthropic", 200_000, true, 15.00, 75.00, 0.95, 4200 * time.Millisecond},
	{epSonnet, "anthropic", 200_000, true, 3.00, 15.00, 0.88, 1800 * time.Millisecond},
	{epMini, "openai", 128_000, true, 0.15, 0.60, 0.62, 900 * time.Millisecond},
	{epFlash, "google", 1_000_000, true, 0.10, 0.40, 0.60, 700 * time.Millisecond},
	{epQwen, "ollama", 32_000, false, 0.00, 0.00, 0.55, 2500 * time.Millisecond},
}

func docCatalog() *domain.Catalog {
	eps := make(map[string]*domain.ModelEndpoint, len(docSpecs))
	// Opus is deliberately absent from the candidate list: it is the route's
	// baseline, and the router appends the baseline as a candidate so that
	// "serve what was asked for" is never an option it removed from itself.
	ids := make([]string, 0, len(docSpecs))
	for _, s := range docSpecs {
		eps[s.id] = s.build()
		if s.id != epOpus {
			ids = append(ids, s.id)
		}
	}
	return &domain.Catalog{
		Version:   "2026-08-04.1",
		Endpoints: eps,
		Aliases:   map[string]string{"claude-sonnet-5": epSonnet},
		Routes: map[string]*domain.Route{
			routeFastCoder: {
				Name:       routeFastCoder,
				Candidates: ids,
				BaselineID: epOpus,
				Require: []domain.Constraint{
					{Capability: "tools"},
					{QualityDim: "coding", QualityMin: 0.60},
				},
				Weights: map[string]float64{
					"quality.coding":        0.40,
					domain.DimCost:          0.30,
					domain.DimLatency:       0.20,
					domain.DimCacheAffinity: 0.10,
				},
				Fallback:    epSonnet,
				MaxAttempts: 3,
			},
		},
	}
}

// docHealth trips the breaker on gemini-flash, as the worked example does.
func docHealth() *domain.Health {
	h := &domain.Health{Endpoints: map[string]domain.EndpointHealth{}}
	for _, s := range docSpecs {
		h.Endpoints[s.id] = domain.EndpointHealth{LatencyEWMA: s.latency}
	}
	h.Endpoints[epFlash] = domain.EndpointHealth{
		CircuitOpen: true,
		LatencyEWMA: 700 * time.Millisecond,
	}
	return h
}

// docRequest is the worked example's request: a coding task, ~8,000 input
// tokens, max_tokens 1500, tools required, previous turn served by Sonnet.
func docRequest() *domain.Request {
	return &domain.Request{
		ID:        "req-1",
		Tenant:    "tenant-42",
		RouteName: routeFastCoder,
		Baseline: domain.Baseline{
			EndpointID: epOpus,
			Mode:       domain.ModeOptimize,
			Source:     "explicit_model",
		},
		Need: domain.Need{Tools: true},
		Estimate: domain.Estimate{
			InputTokens:          8000,
			MaxOutputTokens:      1500,
			ExpectedOutputTokens: 1500,
		},
		SessionKey:       "sess-1",
		PreviousEndpoint: epSonnet,
	}
}

func docPolicy() *domain.Policy {
	return &domain.Policy{
		Version:          "tenant-42.7",
		OptimizationMode: domain.ModeOptimize,
	}
}

func docInput() Input {
	return Input{
		Request: docRequest(),
		Catalog: docCatalog(),
		Policy:  docPolicy(),
		Health:  docHealth(),
	}
}

// --- assertion helpers ---

func rejectionFor(d *domain.Decision, id string) (domain.RejectedCandidate, bool) {
	for _, r := range d.Rejected {
		if r.EndpointID == id {
			return r, true
		}
	}
	return domain.RejectedCandidate{}, false
}

func rankedIDs(d *domain.Decision) []string {
	ids := make([]string, len(d.Ranked))
	for i, c := range d.Ranked {
		ids[i] = c.EndpointID
	}
	return ids
}

func scoreOf(d *domain.Decision, id string) (float64, bool) {
	for _, c := range d.Ranked {
		if c.EndpointID == id {
			return c.Total, true
		}
	}
	return 0, false
}
