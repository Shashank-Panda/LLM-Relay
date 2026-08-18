package tenant

import (
	"errors"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
)

const (
	acmeKey  = "sk-relay-acme-0123456789abcdef"
	otherKey = "sk-relay-globex-fedcba9876543210"
)

func configFor(t *testing.T) *Registry {
	t.Helper()
	yaml := `
default:
  id: default
  name: anonymous
  policy:
    optimization_mode: strict

tenants:
  - id: acme
    name: Acme Corp
    keys:
      - ` + HashKey(acmeKey) + `
    policy:
      optimization_mode: shadow
      max_cost_per_request_usd: 0.50
      quality_floor:
        coding: 0.60

  - id: globex
    name: Globex
    keys:
      - ` + HashKey(otherKey) + `
    policy:
      optimization_mode: strict
`
	r, err := Load(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func TestResolveKey(t *testing.T) {
	r := configFor(t)

	t.Run("a known key resolves to its tenant", func(t *testing.T) {
		got, ok := r.Resolve(acmeKey)
		if !ok {
			t.Fatal("known key rejected")
		}
		if got.ID != "acme" {
			t.Errorf("tenant = %q, want acme", got.ID)
		}
		// The setting this whole phase exists to make usable.
		if got.Policy.OptimizationMode != domain.ModeShadow {
			t.Errorf("mode = %s, want shadow", got.Policy.OptimizationMode)
		}
		if got.Policy.MaxCostPerRequest != 500_000 {
			t.Errorf("MaxCostPerRequest = %s, want $0.50", got.Policy.MaxCostPerRequest)
		}
		if got.Policy.QualityFloor["coding"] != 0.60 {
			t.Errorf("quality floor = %v", got.Policy.QualityFloor)
		}
	})

	// Phase 1 accepted unauthenticated requests. Phase 2 must not break an
	// existing deployment, so no key means the default tenant rather than a 401.
	t.Run("no key is the default tenant, not a rejection", func(t *testing.T) {
		got, ok := r.Resolve("")
		if !ok {
			t.Fatal("an unkeyed request was rejected")
		}
		if got.ID != DefaultID {
			t.Errorf("tenant = %q, want %q", got.ID, DefaultID)
		}
		if got.Policy.OptimizationMode != domain.ModeStrict {
			t.Errorf("default mode = %s, want strict", got.Policy.OptimizationMode)
		}
	})

	// A wrong key is a mistake worth reporting, unlike no key at all.
	t.Run("an unknown key is rejected", func(t *testing.T) {
		if _, ok := r.Resolve("sk-relay-not-a-real-key"); ok {
			t.Error("an unknown key was accepted")
		}
	})

	t.Run("tenants are isolated", func(t *testing.T) {
		got, _ := r.Resolve(otherKey)
		if got.ID != "globex" {
			t.Fatalf("tenant = %q", got.ID)
		}
		if got.Policy.OptimizationMode != domain.ModeStrict {
			t.Errorf("globex inherited acme's mode: %s", got.Policy.OptimizationMode)
		}
	})
}

// A config file that leaks must not be a set of working credentials, so keys
// are stored hashed and the plaintext never appears anywhere in the registry.
func TestKeysAreStoredHashed(t *testing.T) {
	r := configFor(t)

	for hash := range r.byHash {
		if strings.Contains(hash, "sk-") {
			t.Errorf("a plaintext key is stored: %q", hash)
		}
		if !isSHA256Hex(hash) {
			t.Errorf("stored key %q is not a digest", hash)
		}
	}
}

// A plaintext key pasted into the config would simply never match, and the
// operator would debug an authentication failure that looks like a client
// problem. Failing at startup is far cheaper.
func TestPlaintextKeyIsRejectedAtLoad(t *testing.T) {
	_, err := Load(strings.NewReader(`
tenants:
  - id: acme
    keys:
      - sk-relay-plaintext-oops
`))
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LoadError", err)
	}
	if !strings.Contains(le.Error(), "SHA-256") {
		t.Errorf("message = %q, want it to explain how to hash the key", le.Error())
	}
}

// One key authenticating two tenants would attribute cost to whichever happened
// to load last — a silent billing error.
func TestDuplicateKeyAcrossTenantsIsRejected(t *testing.T) {
	h := HashKey(acmeKey)
	_, err := Load(strings.NewReader(`
tenants:
  - id: acme
    keys: [` + h + `]
  - id: globex
    keys: [` + h + `]
`))
	if err == nil {
		t.Fatal("a shared key was accepted")
	}
	if !strings.Contains(err.Error(), "already assigned") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadRejections(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		wantIn string
	}{
		{
			name:   "missing id",
			yaml:   "tenants:\n  - name: nameless\n",
			wantIn: "id is required",
		},
		{
			name:   "unknown optimization mode",
			yaml:   "tenants:\n  - id: a\n    policy:\n      optimization_mode: cheapest\n",
			wantIn: "not one of strict, shadow, optimize",
		},
		{
			name:   "quality floor out of range",
			yaml:   "tenants:\n  - id: a\n    policy:\n      quality_floor:\n        coding: 1.5\n",
			wantIn: "must be between 0 and 1",
		},
		{
			name:   "negative cost cap",
			yaml:   "tenants:\n  - id: a\n    policy:\n      max_cost_per_request_usd: -1\n",
			wantIn: "must not be negative",
		},
		{
			name:   "duplicate tenant id",
			yaml:   "tenants:\n  - id: a\n  - id: a\n",
			wantIn: "duplicate tenant id",
		},
		{
			// Strict decoding. A typo in an access-control file that is silently
			// ignored produces a policy nobody believes is in force.
			name:   "unknown field",
			yaml:   "tenants:\n  - id: a\n    polcy:\n      optimization_mode: shadow\n",
			wantIn: "field polcy not found",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tc.yaml))
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %q, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

// An absent tenants file means Phase 1's behaviour exactly: every request
// anonymous, attributed to the default tenant, nothing rejected.
func TestMissingFileIsNotAnError(t *testing.T) {
	r, err := LoadFile(t.TempDir() + "/does-not-exist.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	got, ok := r.Resolve("")
	if !ok || got.ID != DefaultID {
		t.Errorf("resolved to %+v, want the default tenant", got)
	}
}

func TestEmptyFileIsNotAnError(t *testing.T) {
	r, err := Load(strings.NewReader(""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Default() == nil || r.Default().Policy == nil {
		t.Error("the default tenant is missing a policy")
	}
}

// The zero value is strict. Nobody gets substituted by leaving a field blank.
func TestUnsetModeIsStrict(t *testing.T) {
	r, err := Load(strings.NewReader("tenants:\n  - id: a\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tn, _ := r.ByID("a")
	if tn.Policy.OptimizationMode != domain.ModeStrict {
		t.Errorf("mode = %s, want strict", tn.Policy.OptimizationMode)
	}
}

func TestBearerToken(t *testing.T) {
	tests := map[string]string{
		"Bearer sk-abc":    "sk-abc",
		"bearer sk-abc":    "sk-abc",
		"  Bearer sk-abc ": "sk-abc",
		// curl users routinely send a bare key.
		"sk-abc": "sk-abc",
		"":       "",
		"   ":    "",
	}
	for in, want := range tests {
		if got := BearerToken(in); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIDsAreSorted(t *testing.T) {
	r := configFor(t)
	got := r.IDs()
	if len(got) != 3 {
		t.Fatalf("IDs = %v, want default + 2 tenants", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("IDs are not sorted: %v", got)
		}
	}
}

// --- Phase 3: lever configuration ---

// withLevers wraps a levers block in the smallest valid tenant file.
func withLevers(block string) string {
	return "tenants:\n  - id: t\n    policy:\n      levers:\n" + block
}

func leversOf(t *testing.T, block string) domain.LeverConfig {
	t.Helper()
	r, err := Load(strings.NewReader(withLevers(block)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tn, ok := r.ByID("t")
	if !ok {
		t.Fatal("tenant t is missing")
	}
	return tn.Policy.Levers
}

func TestNoLeverBlockEnablesNothing(t *testing.T) {
	// The same posture as strict mode: a tenant must never discover their
	// requests are being rewritten because a field was left blank.
	r, err := Load(strings.NewReader("tenants:\n  - id: t\n    policy:\n      optimization_mode: shadow\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tn, _ := r.ByID("t")
	if tn.Policy.Levers != (domain.LeverConfig{}) {
		t.Errorf("Levers = %+v, want the zero value", tn.Policy.Levers)
	}
}

func TestRecommendedPreset(t *testing.T) {
	got := leversOf(t, "        preset: recommended\n")

	if got != domain.RecommendedLevers() {
		t.Errorf("preset produced %+v, want %+v", got, domain.RecommendedLevers())
	}
	// The preset enables the two levers that do not change what the model is
	// asked, and leaves the two that do switched off.
	if got.ContextPruning || got.EffortDownshift {
		t.Error("the recommended preset enabled a lever that changes request semantics")
	}
}

func TestAFieldCanTurnAPresetLeverOff(t *testing.T) {
	// The reason these fields are pointers. With a plain bool, false is
	// indistinguishable from unset, and a tenant disabling one lever from a
	// preset would silently keep it.
	got := leversOf(t, "        preset: recommended\n        output_ceiling: false\n")

	if got.OutputCeiling {
		t.Error("output_ceiling: false did not override the preset")
	}
	if !got.CacheBreakpoints {
		t.Error("overriding one lever switched off another")
	}
}

func TestLeverRejections(t *testing.T) {
	tests := map[string]struct {
		block string
		want  string
	}{
		"unknown preset": {"        preset: aggressive\n", "not one of recommended"},
		// A lever that is on but unconfigured would silently never fire, which
		// reads as "optimization is broken" rather than as "a field is missing".
		"breakpoints unconfigured": {
			"        cache_breakpoints: true\n", "needs max_breakpoints"},
		"ceiling unconfigured": {
			"        output_ceiling: true\n", "needs output_ceiling_slack"},
		"effort without a default": {
			"        effort_downshift: true\n", "needs default_effort"},
		"pruning without a window": {
			"        context_pruning: true\n", "needs keep_turns"},
		// Below 1 the ceiling lands under the observed p95 and truncates most
		// responses. Rejected rather than clamped: the operator meant something,
		// and it was not this.
		"slack below one": {
			"        preset: recommended\n        output_ceiling_slack: 0.5\n", "at least 1"},
		"unknown effort": {
			"        effort_downshift: true\n        default_effort: maximum\n",
			"not one of minimal"},
		"negative bound": {
			"        preset: recommended\n        max_breakpoints: -1\n", "must not be negative"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(strings.NewReader(withLevers(tc.block)))
			if err == nil {
				t.Fatalf("loaded a file with %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLeversAreIndependentOfMode(t *testing.T) {
	// The distinction Phase 3 turns on: mode governs whether Relay may serve a
	// different model, levers govern whether it may rewrite the request sent to
	// the model that was asked for. A strict tenant can hold the first line and
	// still take the savings from the second.
	r, err := Load(strings.NewReader(
		"tenants:\n  - id: t\n    policy:\n      optimization_mode: strict\n" +
			"      levers:\n        preset: recommended\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tn, _ := r.ByID("t")

	if tn.Policy.OptimizationMode != domain.ModeStrict {
		t.Errorf("mode = %q, want strict", tn.Policy.OptimizationMode)
	}
	if !tn.Policy.Levers.CacheBreakpoints {
		t.Error("a strict-mode tenant could not enable a request-level lever")
	}
}

func TestUnknownLeverFieldIsRejected(t *testing.T) {
	// Strict decoding: a typo in a file that governs how requests are rewritten
	// must not be silently ignored.
	if _, err := Load(strings.NewReader(withLevers("        cache_breakpoint: true\n"))); err == nil {
		t.Fatal("a misspelled lever field was accepted")
	}
}
