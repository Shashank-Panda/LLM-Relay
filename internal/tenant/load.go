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

	return &Tenant{ID: ty.ID, Name: ty.Name, Policy: pol}
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
