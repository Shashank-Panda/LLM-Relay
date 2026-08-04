package domain

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Constraint is a route-level hard requirement. Constraints eliminate; weights
// rank. Collapsing the two means a depleted candidate set eventually selects
// something that cannot serve the request at all.
type Constraint struct {
	// Capability, when set, requires the named capability:
	// tools | vision | json_schema | reasoning | streaming.
	Capability string

	// QualityDim, when set, requires Quality[QualityDim] >= QualityMin.
	QualityDim string
	QualityMin float64
}

// Route is what a virtual model names: a candidate set plus how to rank it.
type Route struct {
	Name       string
	Candidates []string

	// BaselineID prices the counterfactual for requests that arrive without an
	// explicit model. Empty means savings are unmeasured for this route —
	// which is a different fact from a measured saving of zero, and the two
	// must never be summed.
	BaselineID string

	Require []Constraint

	// Weights maps a scoring dimension to its coefficient. Keys are "cost",
	// "latency", "cache_affinity", or "quality.<dim>". Must sum to 1.
	Weights map[string]float64

	// Fallback is served when filtering eliminates every candidate. Empty
	// means fall back to the baseline instead.
	Fallback    string
	MaxAttempts int
}

const weightSumTolerance = 1e-9

// QualityDim returns the quality dimension this route ranks on — the
// highest-weighted "quality.*" key, ties broken lexicographically for
// determinism. Empty when the route does not score on quality.
func (r *Route) QualityDim() string {
	best, bestW := "", 0.0
	for _, k := range sortedKeys(r.Weights) {
		dim, ok := strings.CutPrefix(k, "quality.")
		if !ok {
			continue
		}
		if w := r.Weights[k]; w > bestW {
			best, bestW = dim, w
		}
	}
	return best
}

func (r *Route) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("route: name is required")
	}
	if len(r.Candidates) == 0 {
		return fmt.Errorf("route %q: at least one candidate is required", r.Name)
	}
	if len(r.Weights) == 0 {
		return fmt.Errorf("route %q: weights are required", r.Name)
	}
	sum := 0.0
	for _, k := range sortedKeys(r.Weights) {
		w := r.Weights[k]
		if w < 0 {
			return fmt.Errorf("route %q: weight %q is negative", r.Name, k)
		}
		if !isKnownDimension(k) {
			return fmt.Errorf("route %q: unknown scoring dimension %q", r.Name, k)
		}
		sum += w
	}
	// A weight set summing to 1.3 produces scores that look comparable and are
	// not, so this is an error rather than a normalization.
	if math.Abs(sum-1.0) > weightSumTolerance {
		return fmt.Errorf("route %q: weights sum to %v, must sum to 1", r.Name, sum)
	}
	return nil
}

const (
	DimCost          = "cost"
	DimLatency       = "latency"
	DimCacheAffinity = "cache_affinity"
	DimQualityPrefix = "quality."
)

func isKnownDimension(k string) bool {
	switch k {
	case DimCost, DimLatency, DimCacheAffinity:
		return true
	}
	return strings.HasPrefix(k, DimQualityPrefix) && len(k) > len(DimQualityPrefix)
}

// Catalog is an immutable, versioned snapshot. The router reads it and never
// mutates it; reloads replace the whole pointer rather than editing in place.
type Catalog struct {
	Version   string
	Endpoints map[string]*ModelEndpoint
	Aliases   map[string]string
	Routes    map[string]*Route
}

func (c *Catalog) Endpoint(id string) (*ModelEndpoint, bool) {
	if c == nil || c.Endpoints == nil {
		return nil, false
	}
	e, ok := c.Endpoints[id]
	return e, ok
}

// Resolve maps a caller-supplied model name through aliases to an endpoint.
func (c *Catalog) Resolve(name string) (*ModelEndpoint, bool) {
	if c == nil {
		return nil, false
	}
	if id, ok := c.Aliases[name]; ok {
		name = id
	}
	return c.Endpoint(name)
}

func (c *Catalog) Route(name string) (*Route, bool) {
	if c == nil || c.Routes == nil {
		return nil, false
	}
	r, ok := c.Routes[name]
	return r, ok
}

// Validate checks the whole snapshot. Called at load time, never on the hot
// path: a catalog that reaches the router is one that already passed.
func (c *Catalog) Validate() error {
	if c == nil {
		return fmt.Errorf("catalog: nil")
	}
	if len(c.Endpoints) == 0 {
		return fmt.Errorf("catalog: no endpoints")
	}
	for _, id := range sortedKeys(c.Endpoints) {
		e := c.Endpoints[id]
		if e.ID != id {
			return fmt.Errorf("catalog: endpoint keyed %q has ID %q", id, e.ID)
		}
		if e.Limits.ContextWindow <= 0 {
			return fmt.Errorf("endpoint %q: context_window must be positive", id)
		}
		if e.Pricing.Input < 0 || e.Pricing.Output < 0 {
			return fmt.Errorf("endpoint %q: pricing must not be negative", id)
		}
		for _, dim := range sortedKeys(e.Quality) {
			if q := e.Quality[dim]; q < 0 || q > 1 {
				return fmt.Errorf("endpoint %q: quality %q is %v, must be 0..1", id, dim, q)
			}
		}
	}
	for _, alias := range sortedKeys(c.Aliases) {
		if _, ok := c.Endpoints[c.Aliases[alias]]; !ok {
			return fmt.Errorf("alias %q points at unknown endpoint %q", alias, c.Aliases[alias])
		}
	}
	for _, name := range sortedKeys(c.Routes) {
		r := c.Routes[name]
		if r.Name != name {
			return fmt.Errorf("catalog: route keyed %q has name %q", name, r.Name)
		}
		if err := r.Validate(); err != nil {
			return err
		}
		for _, id := range r.Candidates {
			if _, ok := c.Endpoints[id]; !ok {
				return fmt.Errorf("route %q: unknown candidate %q", name, id)
			}
		}
		for _, id := range []string{r.Fallback, r.BaselineID} {
			if id == "" {
				continue
			}
			if _, ok := c.Endpoints[id]; !ok {
				return fmt.Errorf("route %q: unknown endpoint %q", name, id)
			}
		}
	}
	return nil
}

// sortedKeys returns map keys in a stable order.
//
// Used everywhere the router touches a map. Float addition is not associative,
// so summing weighted components in Go's randomized map order would make
// identical inputs produce totals differing in the last ulp — and a router
// that answers differently for the same input cannot be tested or trusted.
func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// SortedKeys exposes sortedKeys to sibling packages that must iterate
// deterministically for the same reason.
func SortedKeys[V any](m map[string]V) []string { return sortedKeys(m) }
