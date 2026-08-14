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
