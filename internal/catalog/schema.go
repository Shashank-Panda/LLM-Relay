package catalog

// The types in this file mirror the YAML file format, not the domain model.
//
// Keeping them separate costs a mapping function and buys three things:
//
//  1. The file format is an operator-facing interface. It should be able to
//     change — a friendlier price notation, a renamed field with a compatibility
//     shim — without touching domain types, and domain types should be able to
//     change without breaking every catalog file in production.
//
//  2. Domain types stay free of yaml tags. domain.ModelEndpoint describes what
//     a model endpoint *is*; it should not also encode how one happens to be
//     spelled on disk.
//
//  3. This is the layer where human-friendly representations become machine
//     ones: dollars become integer micro-dollars, a date string becomes a
//     time.Time, an absent lifecycle becomes an explicit "ga". Doing that
//     conversion in exactly one place means there is exactly one place where it
//     can be wrong.

type file struct {
	Version   string            `yaml:"version"`
	Endpoints []endpointYAML    `yaml:"endpoints"`
	Aliases   map[string]string `yaml:"aliases"`
	Routes    []routeYAML       `yaml:"routes"`
}

type endpointYAML struct {
	ID            string `yaml:"id"`
	Provider      string `yaml:"provider"`
	Model         string `yaml:"model"`
	Deployment    string `yaml:"deployment"`
	CredentialRef string `yaml:"credential_ref"`
	BaseURL       string `yaml:"base_url"`

	Capabilities capabilitiesYAML   `yaml:"capabilities"`
	Limits       limitsYAML         `yaml:"limits"`
	Pricing      pricingYAML        `yaml:"pricing"`
	Quality      map[string]float64 `yaml:"quality"`
	Lifecycle    lifecycleYAML      `yaml:"lifecycle"`
}

type capabilitiesYAML struct {
	Streaming  bool `yaml:"streaming"`
	Tools      bool `yaml:"tools"`
	JSONSchema bool `yaml:"json_schema"`
	Vision     bool `yaml:"vision"`
	Reasoning  bool `yaml:"reasoning"`
}

type limitsYAML struct {
	ContextWindow   int `yaml:"context_window"`
	MaxOutputTokens int `yaml:"max_output_tokens"`
}

// pricingYAML quotes prices in US dollars per million tokens, because that is
// how every provider publishes them and a catalog an operator cannot check
// against a pricing page by eye is a catalog that drifts.
//
// Source and VerifiedOn are not decoration. A wrong price does not fail — it
// routes, confidently, to the wrong endpoint, and quietly corrupts every
// savings figure computed against it. Requiring an attestation for any non-zero
// price makes staleness a startup error instead of a slow leak.
type pricingYAML struct {
	Input       float64 `yaml:"input"`
	CachedInput float64 `yaml:"cached_input"`
	Output      float64 `yaml:"output"`
	Reasoning   float64 `yaml:"reasoning"`

	Source     string `yaml:"source"`
	VerifiedOn string `yaml:"verified_on"` // YYYY-MM-DD
}

func (p pricingYAML) isFree() bool {
	return p.Input == 0 && p.CachedInput == 0 && p.Output == 0 && p.Reasoning == 0
}

type lifecycleYAML struct {
	Status      string `yaml:"status"`
	Replacement string `yaml:"replacement"`
}

type routeYAML struct {
	Name        string             `yaml:"name"`
	Candidates  []string           `yaml:"candidates"`
	Baseline    string             `yaml:"baseline"`
	Require     []constraintYAML   `yaml:"require"`
	Weights     map[string]float64 `yaml:"weights"`
	Fallback    string             `yaml:"fallback"`
	MaxAttempts int                `yaml:"max_attempts"`
}

// constraintYAML is deliberately structured rather than an expression string.
//
// An earlier draft of the docs wrote requirements as `quality.coding: ">= 0.60"`,
// which reads well and means writing an expression parser, inventing an operator
// grammar, and deciding what `>= abc` does at load time. A constraint that
// decides who may spend money is the last place to accept a stringly-typed DSL.
type constraintYAML struct {
	Capability string  `yaml:"capability"`
	Quality    string  `yaml:"quality"`
	Min        float64 `yaml:"min"`
}
