// Package catalog loads the model catalog from YAML into an immutable,
// validated domain.Catalog snapshot.
//
// The catalog is data, not code: adding a model, repricing one, or retiring one
// is a config change reviewed like any other, not a deploy. That only holds if
// the loader is strict. A catalog that loads with a typo silently ignored is
// worse than one that fails, because the router will keep making confident
// decisions from a field the operator believes they set.
package catalog

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// Options configures the load. The zero value performs no freshness check and
// uses the wall clock.
type Options struct {
	// MaxPriceAge rejects any priced endpoint whose pricing.verified_on is
	// older than this. Zero disables the check.
	//
	// Stale prices are the failure mode nobody notices: nothing errors, the
	// router simply optimizes against numbers that stopped being true, and the
	// savings ledger reports the difference as fact.
	MaxPriceAge time.Duration

	// Now is injected so that freshness is testable without a fake clock
	// package and without tests that rot as the calendar advances.
	// Zero means time.Now().
	Now time.Time
}

// DefaultOptions requires pricing to have been verified within 90 days.
func DefaultOptions() Options {
	return Options{MaxPriceAge: 90 * 24 * time.Hour}
}

func (o Options) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

const verifiedOnLayout = "2006-01-02"

// Problem is a single defect, located by a path into the file.
type Problem struct {
	Path string
	Msg  string
}

// LoadError reports every problem found, not just the first.
//
// An operator editing a catalog wants the whole list in one pass. Failing on
// the first defect turns a five-minute edit into a five-round game of
// whack-a-mole, and the rounds are separated by a process restart.
type LoadError struct {
	Problems []Problem
}

func (e *LoadError) Error() string {
	if len(e.Problems) == 1 {
		return fmt.Sprintf("catalog: %s: %s", e.Problems[0].Path, e.Problems[0].Msg)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "catalog: %d problems:", len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s: %s", p.Path, p.Msg)
	}
	return b.String()
}

// collector accumulates problems during conversion.
type collector struct{ problems []Problem }

func (c *collector) add(path, format string, args ...any) {
	c.problems = append(c.problems, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (c *collector) err() error {
	if len(c.problems) == 0 {
		return nil
	}
	return &LoadError{Problems: c.problems}
}

// LoadFile reads and validates a catalog from disk.
func LoadFile(path string, opts Options) (*domain.Catalog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer f.Close()

	cat, err := Load(f, opts)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", path, err)
	}
	return cat, nil
}

// Load reads and validates a catalog from a reader.
//
// Nothing partially valid escapes: the sequence is decode, convert, validate,
// and only a catalog that clears all three is returned. A caller holding a
// non-nil *domain.Catalog is holding one the router can trust, which is what
// lets the router itself skip defensive checks on the hot path.
func Load(r io.Reader, opts Options) (*domain.Catalog, error) {
	dec := yaml.NewDecoder(r)

	// Strict decoding. Without it, `capabilties: {tools: true}` parses happily,
	// the endpoint reports no tool support, and the router silently stops
	// selecting it — a routing bug that looks like a modelling disagreement and
	// is invisible in every log.
	dec.KnownFields(true)

	var f file
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &LoadError{Problems: []Problem{{Path: "catalog", Msg: "file is empty"}}}
		}
		return nil, fmt.Errorf("catalog: parse: %w", err)
	}

	c := &collector{}
	cat := convert(&f, opts, c)
	if err := c.err(); err != nil {
		return nil, err
	}

	// The final gate is the same Validate the router's own tests rely on, so
	// there is one definition of a well-formed catalog rather than two that can
	// drift apart.
	if err := cat.Validate(); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	return cat, nil
}

func convert(f *file, opts Options, c *collector) *domain.Catalog {
	cat := &domain.Catalog{
		Version:   f.Version,
		Endpoints: make(map[string]*domain.ModelEndpoint, len(f.Endpoints)),
		Aliases:   make(map[string]string, len(f.Aliases)),
		Routes:    make(map[string]*domain.Route, len(f.Routes)),
	}

	if f.Version == "" {
		c.add("version", "is required; decisions record the catalog version so they can be replayed")
	}
	if len(f.Endpoints) == 0 {
		c.add("endpoints", "at least one endpoint is required")
	}

	for i, e := range f.Endpoints {
		path := fmt.Sprintf("endpoints[%d]", i)
		if e.ID != "" {
			path = fmt.Sprintf("endpoints[%d] %q", i, e.ID)
		}
		ep := convertEndpoint(e, path, opts, c)
		if ep == nil {
			continue
		}
		// A duplicate ID would silently overwrite — including its prices.
		if _, dup := cat.Endpoints[ep.ID]; dup {
			c.add(path, "duplicate endpoint id")
			continue
		}
		cat.Endpoints[ep.ID] = ep
	}

	for _, alias := range domain.SortedKeys(f.Aliases) {
		target := f.Aliases[alias]
		path := fmt.Sprintf("aliases[%q]", alias)
		// Catalog.Resolve consults aliases first, so an alias sharing a name
		// with an endpoint would silently redirect traffic away from it.
		if _, clash := cat.Endpoints[alias]; clash {
			c.add(path, "alias shadows the endpoint of the same id")
			continue
		}
		if target == "" {
			c.add(path, "target is empty")
			continue
		}
		cat.Aliases[alias] = target
	}

	for i, r := range f.Routes {
		path := fmt.Sprintf("routes[%d]", i)
		if r.Name != "" {
			path = fmt.Sprintf("routes[%d] %q", i, r.Name)
		}
		rt := convertRoute(r, path, c)
		if rt == nil {
			continue
		}
		if _, dup := cat.Routes[rt.Name]; dup {
			c.add(path, "duplicate route name")
			continue
		}
		cat.Routes[rt.Name] = rt
	}

	return cat
}

func convertEndpoint(e endpointYAML, path string, opts Options, c *collector) *domain.ModelEndpoint {
	if e.ID == "" {
		c.add(path, "id is required")
		return nil
	}
	if e.Provider == "" {
		c.add(path, "provider is required")
	}
	if e.Model == "" {
		c.add(path, "model is required")
	}
	if e.Limits.ContextWindow <= 0 {
		c.add(path, "limits.context_window must be positive")
	}
	// A base_url without a scheme produces a request that fails at dial time
	// with an error naming neither the endpoint nor the catalog. Cheaper to
	// reject the string here, where the file path is still in hand.
	if e.BaseURL != "" && !strings.HasPrefix(e.BaseURL, "http://") && !strings.HasPrefix(e.BaseURL, "https://") {
		c.add(path, "base_url %q must start with http:// or https://", e.BaseURL)
	}

	status := domain.LifecycleStatus(e.Lifecycle.Status)
	if e.Lifecycle.Status == "" {
		status = domain.StatusGA
	} else if !validStatus(status) {
		c.add(path, "lifecycle.status %q is not one of ga, preview, deprecated, retired",
			e.Lifecycle.Status)
	}

	for _, dim := range domain.SortedKeys(e.Quality) {
		if q := e.Quality[dim]; q < 0 || q > 1 {
			c.add(path, "quality.%s is %v, must be between 0 and 1", dim, q)
		}
	}

	checkPricing(e, path, opts, c)

	return &domain.ModelEndpoint{
		ID:            e.ID,
		Provider:      domain.ProviderID(e.Provider),
		Model:         e.Model,
		Deployment:    e.Deployment,
		CredentialRef: e.CredentialRef,
		BaseURL:       strings.TrimRight(e.BaseURL, "/"),
		Capabilities: domain.Capabilities{
			Streaming:  e.Capabilities.Streaming,
			Tools:      e.Capabilities.Tools,
			JSONSchema: e.Capabilities.JSONSchema,
			Vision:     e.Capabilities.Vision,
			Reasoning:  e.Capabilities.Reasoning,
		},
		Limits: domain.Limits{
			ContextWindow:   e.Limits.ContextWindow,
			MaxOutputTokens: e.Limits.MaxOutputTokens,
		},
		Pricing: domain.Pricing{
			Input:       dollarsPerMillion(e.Pricing.Input),
			CachedInput: dollarsPerMillion(e.Pricing.CachedInput),
			Output:      dollarsPerMillion(e.Pricing.Output),
			Reasoning:   dollarsPerMillion(e.Pricing.Reasoning),
		},
		Quality:   copyFloats(e.Quality),
		Lifecycle: domain.Lifecycle{Status: status, Replacement: e.Lifecycle.Replacement},
	}
}

func checkPricing(e endpointYAML, path string, opts Options, c *collector) {
	for name, v := range map[string]float64{
		"input": e.Pricing.Input, "cached_input": e.Pricing.CachedInput,
		"output": e.Pricing.Output, "reasoning": e.Pricing.Reasoning,
	} {
		if v < 0 {
			c.add(path, "pricing.%s is negative", name)
		}
	}

	// A free endpoint — a local model, say — has nothing to attest to.
	if e.Pricing.isFree() {
		return
	}

	if e.Pricing.VerifiedOn == "" {
		c.add(path, "pricing.verified_on is required for a priced endpoint; "+
			"an unattested price is one nobody has agreed to be responsible for")
		return
	}

	verified, err := time.Parse(verifiedOnLayout, e.Pricing.VerifiedOn)
	if err != nil {
		c.add(path, "pricing.verified_on %q is not a YYYY-MM-DD date", e.Pricing.VerifiedOn)
		return
	}

	now := opts.now()
	if verified.After(now.Add(24 * time.Hour)) {
		c.add(path, "pricing.verified_on %s is in the future", e.Pricing.VerifiedOn)
		return
	}
	if opts.MaxPriceAge > 0 && now.Sub(verified) > opts.MaxPriceAge {
		c.add(path, "pricing.verified_on %s is older than %s; re-verify against %s",
			e.Pricing.VerifiedOn, opts.MaxPriceAge, sourceOrProviderPage(e.Pricing.Source))
	}
}

func sourceOrProviderPage(src string) string {
	if src == "" {
		return "the provider's pricing page"
	}
	return src
}

func convertRoute(r routeYAML, path string, c *collector) *domain.Route {
	if r.Name == "" {
		c.add(path, "name is required")
		return nil
	}
	if len(r.Candidates) == 0 {
		c.add(path, "at least one candidate is required")
	}
	if len(r.Weights) == 0 {
		c.add(path, "weights are required")
	}

	seen := make(map[string]bool, len(r.Candidates))
	candidates := make([]string, 0, len(r.Candidates))
	for _, id := range r.Candidates {
		// A duplicate candidate would be filtered and scored twice, appearing
		// in the ranking twice and distorting the min-max normalization that
		// every other candidate's cost score is computed against.
		if seen[id] {
			c.add(path, "candidate %q is listed more than once", id)
			continue
		}
		seen[id] = true
		candidates = append(candidates, id)
	}

	constraints := make([]domain.Constraint, 0, len(r.Require))
	for i, req := range r.Require {
		rp := fmt.Sprintf("%s.require[%d]", path, i)
		switch {
		case req.Capability != "" && req.Quality != "":
			c.add(rp, "set either capability or quality, not both")
		case req.Capability != "":
			if !validCapability(req.Capability) {
				c.add(rp, "capability %q is not one of %s",
					req.Capability, strings.Join(knownCapabilities, ", "))
				continue
			}
			constraints = append(constraints, domain.Constraint{Capability: req.Capability})
		case req.Quality != "":
			if req.Min <= 0 || req.Min > 1 {
				c.add(rp, "min is %v, must be between 0 and 1", req.Min)
				continue
			}
			constraints = append(constraints, domain.Constraint{
				QualityDim: req.Quality, QualityMin: req.Min,
			})
		default:
			c.add(rp, "requires either capability or quality")
		}
	}

	rt := &domain.Route{
		Name:        r.Name,
		Candidates:  candidates,
		BaselineID:  r.Baseline,
		Require:     constraints,
		Weights:     copyFloats(r.Weights),
		Fallback:    r.Fallback,
		MaxAttempts: r.MaxAttempts,
		Cache:       convertRouteCache(r.Cache, path, c),
	}
	if rt.MaxAttempts == 0 {
		rt.MaxAttempts = 1
	}

	// Surface weight problems here, with a file path attached, rather than
	// letting Validate report them later without one.
	if err := rt.Validate(); err != nil && len(r.Weights) > 0 && len(candidates) > 0 {
		c.add(path, "%s", strings.TrimPrefix(err.Error(), fmt.Sprintf("route %q: ", r.Name)))
	}

	return rt
}

// convertRouteCache validates one route's cache block.
//
// Settings on a disabled cache are rejected rather than ignored. An operator who
// wrote a TTL and no `enabled: true` believes caching is on; silently accepting
// the file means they find out it is not from a savings report months later.
func convertRouteCache(y routeCacheYAML, path string, c *collector) domain.RouteCache {
	out := domain.RouteCache{Enabled: y.Enabled, AllowTemperature: y.AllowTemperature}

	if !y.Enabled && (y.TTL != "" || y.AllowTemperature) {
		c.add(path+".cache", "configured but not enabled; set enabled: true or remove the block")
		return domain.RouteCache{}
	}
	if y.TTL == "" {
		return out
	}

	ttl, err := time.ParseDuration(y.TTL)
	switch {
	case err != nil:
		c.add(path+".cache.ttl", "%q is not a duration such as \"5m\" or \"1h\"", y.TTL)
	case ttl <= 0:
		c.add(path+".cache.ttl", "must be positive; omit it to use the %s default",
			domain.DefaultCacheTTL)
	case ttl > maxCacheTTL:
		// A day-long response cache is almost always a mistake, and the mistake
		// is invisible: everything works and some answers are stale. Making it
		// an error means the operator who genuinely wants it has to say so by
		// changing this constant, in a review.
		c.add(path+".cache.ttl", "%s exceeds the %s maximum; an answer older than that "+
			"should be re-generated rather than replayed", ttl, maxCacheTTL)
	default:
		out.TTL = ttl
	}
	return out
}

// maxCacheTTL bounds how stale a replayed answer may be.
const maxCacheTTL = 24 * time.Hour

// knownCapabilities is the closed set a route may require. An unrecognised name
// is an error rather than a no-op: a typo in a constraint must not silently
// disable the constraint.
var knownCapabilities = []string{"json_schema", "reasoning", "streaming", "tools", "vision"}

func validCapability(name string) bool {
	return slices.Contains(knownCapabilities, name)
}

func validStatus(s domain.LifecycleStatus) bool {
	switch s {
	case domain.StatusGA, domain.StatusPreview, domain.StatusDeprecated, domain.StatusRetired:
		return true
	}
	return false
}

// dollarsPerMillion converts a published per-million-token price to the
// integer micro-dollar rate the router uses.
//
// Rounded, not truncated. 0.15 is not representable in binary floating point;
// 0.15*1e6 evaluates to 150000.00000000003, and a truncating conversion would
// shave a micro-dollar off prices ending in 5 — small, systematic, and in the
// direction that understates cost.
func dollarsPerMillion(usd float64) domain.Rate {
	if usd <= 0 {
		return 0
	}
	return domain.Rate(math.Round(usd * 1_000_000))
}

// copyFloats defensively copies a decoded map. The catalog is handed to the
// router as an immutable snapshot; sharing backing maps with the decoder would
// make that a promise rather than a property.
func copyFloats(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(m)
}
