package tenant

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// The wire types, kept separate from the domain for the same reason the catalog
// does it: the file is an operator-facing interface and should be able to
// change without dragging the domain model with it.

type file struct {
	Default *tenantYAML  `yaml:"default"`
	Tenants []tenantYAML `yaml:"tenants"`
}

type tenantYAML struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`

	// Keys are SHA-256 hex digests, never plaintext. The loader rejects
	// anything that does not look like one, so a plaintext key pasted here
	// fails loudly at startup rather than quietly never matching.
	Keys []string `yaml:"keys"`

	Policy policyYAML `yaml:"policy"`
}

type policyYAML struct {
	// OptimizationMode is the setting this whole phase exists to make usable:
	// strict | shadow | optimize.
	OptimizationMode string `yaml:"optimization_mode"`

	Allow   []string `yaml:"allow"`
	Deny    []string `yaml:"deny"`
	Regions []string `yaml:"regions"`

	// MaxCostPerRequestUSD is quoted in dollars because that is how an operator
	// thinks about it; it becomes integer micro-dollars here.
	MaxCostPerRequestUSD float64 `yaml:"max_cost_per_request_usd"`

	QualityFloor map[string]float64 `yaml:"quality_floor"`

	// Levers selects the request optimizations this tenant has agreed to.
	//
	// Separate from optimization_mode, and the separation is the point. Mode
	// governs whether Relay may serve a *different model*; levers govern whether
	// it may rewrite the request it sends to the model the tenant asked for.
	// A strict-mode tenant can hold the first line and still take the savings
	// from the second, which is the whole reason the levers ship before routing
	// does (ADR-0008).
	Levers leversYAML `yaml:"levers"`
}

// leversYAML mirrors domain.LeverConfig, with a preset shorthand.
//
// The zero value enables nothing, matching the domain type: a tenant should
// never discover their requests are being rewritten because a field was left
// blank.
type leversYAML struct {
	// Preset applies a named starting point that individual fields then
	// override. "recommended" enables the two semantics-preserving levers.
	Preset string `yaml:"preset"`

	CacheBreakpoints *bool `yaml:"cache_breakpoints"`
	EffortDownshift  *bool `yaml:"effort_downshift"`
	OutputCeiling    *bool `yaml:"output_ceiling"`
	ContextPruning   *bool `yaml:"context_pruning"`

	MinCacheableTokens int `yaml:"min_cacheable_tokens"`
	MaxBreakpoints     int `yaml:"max_breakpoints"`

	DefaultEffort string `yaml:"default_effort"`

	OutputCeilingSlack float64 `yaml:"output_ceiling_slack"`
	MinOutputCeiling   int     `yaml:"min_output_ceiling"`
	MaxOutputCeiling   int     `yaml:"max_output_ceiling"`

	KeepTurns int `yaml:"keep_turns"`
}

// Problem is one defect, located by a path into the file.
type Problem struct {
	Path string
	Msg  string
}

// LoadError reports every problem found, not just the first — an operator
// editing tenants wants the whole list in one pass.
type LoadError struct{ Problems []Problem }

func (e *LoadError) Error() string {
	if len(e.Problems) == 1 {
		return fmt.Sprintf("tenants: %s: %s", e.Problems[0].Path, e.Problems[0].Msg)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tenants: %d problems:", len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s: %s", p.Path, p.Msg)
	}
	return b.String()
}

// LoadFile reads a tenant registry from disk.
//
// A missing file is not an error: it means no tenants are configured, every
// request is anonymous, and everything is attributed to the default tenant.
// That is exactly Phase 1's behaviour, so an existing deployment keeps working
// without touching its configuration.
func LoadFile(path string) (*Registry, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(nil), nil
	}
	if err != nil {
		return nil, fmt.Errorf("tenants: %w", err)
	}
	defer f.Close()

	r, err := Load(f)
	if err != nil {
		return nil, fmt.Errorf("tenants %s: %w", path, err)
	}
	return r, nil
}

// Load reads a registry from a reader.
func Load(r io.Reader) (*Registry, error) {
	dec := yaml.NewDecoder(r)
	// Strict, for the same reason the catalog is: a typo in an access-control
	// file that is silently ignored produces a tenant whose policy is not what
	// anybody believes it to be.
	dec.KnownFields(true)

	var f file
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return New(nil), nil
		}
		return nil, fmt.Errorf("parse: %w", err)
	}

	var problems []Problem
	add := func(path, format string, args ...any) {
		problems = append(problems, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
	}

	var def *Tenant
	if f.Default != nil {
		def = convert(*f.Default, "default", add)
		if def != nil && def.ID == "" {
			def.ID = DefaultID
		}
	}

	reg := New(def)

	seenID := map[string]bool{}
	seenKey := map[string]string{}

	for i, ty := range f.Tenants {
		path := fmt.Sprintf("tenants[%d]", i)
		if ty.ID != "" {
			path = fmt.Sprintf("tenants[%d] %q", i, ty.ID)
		}

		t := convert(ty, path, add)
		if t == nil {
			continue
		}
		if seenID[t.ID] {
			add(path, "duplicate tenant id")
			continue
		}
		seenID[t.ID] = true
		reg.byID[t.ID] = t

		for j, k := range ty.Keys {
			kp := fmt.Sprintf("%s.keys[%d]", path, j)
			hash := strings.ToLower(strings.TrimSpace(k))

			if !isSHA256Hex(hash) {
				// Almost certainly a plaintext key pasted in. Failing loudly is
				// far better than accepting it, since it would simply never
				// match and the operator would debug an authentication failure
				// that looks like a client problem.
				add(kp, "is not a SHA-256 hex digest; store the hash, not the key "+
					"(printf %%s KEY | sha256sum)")
				continue
			}
			if owner, dup := seenKey[hash]; dup {
				// One key authenticating two tenants would attribute cost to
				// whichever happened to be loaded last.
				add(kp, "key is already assigned to tenant %q", owner)
				continue
			}
			seenKey[hash] = t.ID
			reg.addKey(hash, t)
		}
	}

	if len(problems) > 0 {
		return nil, &LoadError{Problems: problems}
	}
	return reg, nil
}

func convert(ty tenantYAML, path string, add func(string, string, ...any)) *Tenant {
	if ty.ID == "" && path != "default" {
		add(path, "id is required")
		return nil
	}

	pol := &domain.Policy{
		Version:      "file",
		Allow:        ty.Policy.Allow,
		Deny:         ty.Policy.Deny,
		Regions:      ty.Policy.Regions,
		QualityFloor: ty.Policy.QualityFloor,
	}

	switch m := domain.BaselineMode(ty.Policy.OptimizationMode); {
	case ty.Policy.OptimizationMode == "":
		// The zero value is strict. Nobody gets substituted by leaving a field
		// blank — that is the whole reason the default is what it is.
		pol.OptimizationMode = domain.ModeStrict
	case m == domain.ModeStrict, m == domain.ModeShadow, m == domain.ModeOptimize:
		pol.OptimizationMode = m
	default:
		add(path+".policy.optimization_mode",
			"%q is not one of strict, shadow, optimize", ty.Policy.OptimizationMode)
	}

	if usd := ty.Policy.MaxCostPerRequestUSD; usd < 0 {
		add(path+".policy.max_cost_per_request_usd", "must not be negative")
	} else if usd > 0 {
		pol.MaxCostPerRequest = domain.Money(usd * 1_000_000)
	}

	for dim, v := range ty.Policy.QualityFloor {
		if v < 0 || v > 1 {
			add(fmt.Sprintf("%s.policy.quality_floor.%s", path, dim),
				"is %v, must be between 0 and 1", v)
		}
	}

	pol.Levers = convertLevers(ty.Policy.Levers, path+".policy.levers", add)

	return &Tenant{ID: ty.ID, Name: ty.Name, Policy: pol}
}

// convertLevers turns the file's lever block into a bounded LeverConfig.
//
// Every bound is applied here rather than at use, because "bounded by policy"
// has to mean bounded at the edge. A lever that trusts its own configuration and
// clamps at the point of use has as many places to be wrong as it has callers.
func convertLevers(y leversYAML, path string, add func(string, string, ...any)) domain.LeverConfig {
	var cfg domain.LeverConfig

	switch y.Preset {
	case "":
	case "recommended":
		cfg = domain.RecommendedLevers()
	case "none":
		// Explicit and equivalent to the zero value. Worth accepting so an
		// operator can state the intent rather than express it by omission.
	default:
		add(path+".preset", "%q is not one of recommended, none", y.Preset)
	}

	// Pointers so that `output_ceiling: false` can switch a preset's lever off.
	// With a plain bool, false is indistinguishable from unset, and a tenant
	// trying to disable one lever from a preset would silently keep it.
	setBool(&cfg.CacheBreakpoints, y.CacheBreakpoints)
	setBool(&cfg.EffortDownshift, y.EffortDownshift)
	setBool(&cfg.OutputCeiling, y.OutputCeiling)
	setBool(&cfg.ContextPruning, y.ContextPruning)

	setPositive(&cfg.MinCacheableTokens, y.MinCacheableTokens, path+".min_cacheable_tokens", add)
	setPositive(&cfg.MaxBreakpoints, y.MaxBreakpoints, path+".max_breakpoints", add)
	setPositive(&cfg.MinOutputCeiling, y.MinOutputCeiling, path+".min_output_ceiling", add)
	setPositive(&cfg.MaxOutputCeiling, y.MaxOutputCeiling, path+".max_output_ceiling", add)
	setPositive(&cfg.KeepTurns, y.KeepTurns, path+".keep_turns", add)

	if y.DefaultEffort != "" {
		e := domain.ReasoningEffort(y.DefaultEffort)
		if !e.Valid() {
			add(path+".default_effort", "%q is not one of minimal, low, medium, high",
				y.DefaultEffort)
		} else {
			cfg.DefaultEffort = e
		}
	}

	if s := y.OutputCeilingSlack; s != 0 {
		if s < 1 {
			// Below 1 the ceiling lands under the observed p95, which truncates
			// the majority of responses on that route. Rejected rather than
			// clamped: the operator meant something, and it was not this.
			add(path+".output_ceiling_slack",
				"is %v; must be at least 1, or the ceiling truncates most responses", s)
		} else {
			cfg.OutputCeilingSlack = s
		}
	}

	// Levers that are on but unconfigured would silently never fire, which
	// reads as "optimization is broken" rather than as "a field is missing".
	if cfg.CacheBreakpoints && (cfg.MaxBreakpoints <= 0 || cfg.MinCacheableTokens <= 0) {
		add(path, "cache_breakpoints needs max_breakpoints and min_cacheable_tokens "+
			"(or preset: recommended)")
	}
	if cfg.OutputCeiling && cfg.OutputCeilingSlack < 1 {
		add(path, "output_ceiling needs output_ceiling_slack >= 1 (or preset: recommended)")
	}
	if cfg.EffortDownshift && !cfg.DefaultEffort.Valid() {
		add(path, "effort_downshift needs default_effort")
	}
	if cfg.ContextPruning && cfg.KeepTurns <= 0 {
		add(path, "context_pruning needs keep_turns")
	}

	return cfg
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

func setPositive(dst *int, v int, path string, add func(string, string, ...any)) {
	switch {
	case v == 0: // unset; keep whatever the preset supplied
	case v < 0:
		add(path, "must not be negative")
	default:
		*dst = v
	}
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
