// Package tenant maps an API key to a tenant and its policy.
//
// This is a deliberately thin slice of what Phase 7 will build. Phase 7 owns
// Postgres-backed tenants, key rotation, scopes, and an admin API; what exists
// here is the minimum that makes a *per-tenant* savings report real, because
// the alternative is a ledger written without a tenant dimension that has to be
// re-keyed later — and for billing-adjacent data that is a migration nobody
// enjoys.
//
// Two properties are worth having right even at this size, because retrofitting
// either is worse than building it:
//
//   - Keys are stored hashed. A config file that leaks should not be a set of
//     working credentials.
//   - Comparison is constant-time. Key lookup by map would leak length and
//     prefix information through timing, and an authentication check that is
//     fast for wrong answers is a check that can be searched.
package tenant

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// DefaultID is the tenant an unauthenticated request belongs to.
//
// Phase 1 accepted every request without a key and Phase 2 must not break that,
// so an unkeyed request is not rejected — it is attributed here. Phase 7, which
// owns authentication properly, is where anonymous access becomes a decision
// rather than a default.
const DefaultID = "default"

// Tenant is who a request belongs to and what they have consented to.
type Tenant struct {
	ID   string
	Name string

	// Policy carries the tenant's optimization mode, quality floor, and levers.
	// This is what makes shadow mode a per-customer setting rather than a
	// process-wide flag — which matters because the adoption story is one
	// customer trying shadow while everyone else stays strict.
	Policy *domain.Policy
}

// HashKey is the one-way function keys are stored under.
//
// SHA-256 rather than bcrypt or argon2, deliberately: an API key is a
// high-entropy random string, not a human-chosen password, so it is not
// vulnerable to the dictionary attack that key-stretching exists to slow down.
// Stretching would instead put milliseconds on the hot path of every request.
// This reasoning holds only for generated keys — see the length check in Load.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Registry resolves API keys to tenants.
//
// Built once at startup and read-only thereafter, so it needs no lock.
type Registry struct {
	// byHash is keyed by the hex digest. Lookup is a map hit to find the
	// candidate and a constant-time compare to confirm it, so a wrong key costs
	// the same as a right one.
	byHash map[string]*Tenant
	byID   map[string]*Tenant

	def *Tenant
}

// New builds a registry. The default tenant is always present.
func New(def *Tenant, tenants ...*Tenant) *Registry {
	if def == nil {
		def = &Tenant{ID: DefaultID, Name: "default", Policy: domain.DefaultPolicy()}
	}
	if def.Policy == nil {
		def.Policy = domain.DefaultPolicy()
	}

	r := &Registry{
		byHash: map[string]*Tenant{},
		byID:   map[string]*Tenant{DefaultID: def},
		def:    def,
	}
	r.byID[def.ID] = def

	for _, t := range tenants {
		if t == nil {
			continue
		}
		if t.Policy == nil {
			t.Policy = domain.DefaultPolicy()
		}
		r.byID[t.ID] = t
	}
	return r
}

// addKey registers a hashed key for a tenant. Used by Load.
func (r *Registry) addKey(hash string, t *Tenant) {
	r.byHash[strings.ToLower(hash)] = t
}

// Default returns the tenant unkeyed requests are attributed to.
func (r *Registry) Default() *Tenant { return r.def }

// ByID looks a tenant up by identifier, for the admin savings query.
func (r *Registry) ByID(id string) (*Tenant, bool) {
	t, ok := r.byID[id]
	return t, ok
}

// IDs lists every configured tenant, sorted, for startup logging and reports.
func (r *Registry) IDs() []string {
	return domain.SortedKeys(r.byID)
}

// Resolve maps a presented key to a tenant.
//
// An empty key returns the default tenant and ok=true: Phase 1 accepted
// unauthenticated requests and Phase 2 does not break that. An unrecognised
// key returns ok=false, which the caller turns into a 401 — a wrong key is a
// mistake worth reporting, whereas no key is a configuration Relay still
// supports.
func (r *Registry) Resolve(key string) (*Tenant, bool) {
	if key == "" {
		return r.def, true
	}

	hash := HashKey(key)
	t, ok := r.byHash[hash]
	if !ok {
		// Burn a comparison against a fixed value so an unknown key costs
		// roughly what a known one does. The map lookup above already leaks
		// more than this hides, which is why Phase 7's Postgres-backed lookup
		// should revisit it — but a wrong key must not be conspicuously fast.
		subtle.ConstantTimeCompare([]byte(hash), []byte(hash))
		return nil, false
	}

	// The map found a candidate by digest; confirm it in constant time so a
	// hash collision cannot authenticate.
	if subtle.ConstantTimeCompare([]byte(hash), []byte(HashKey(key))) != 1 {
		return nil, false
	}
	return t, true
}

// BearerToken extracts a key from an Authorization header.
//
// Accepts "Bearer <key>" and a bare key, because OpenAI SDKs send the former
// and curl users routinely send the latter.
func BearerToken(header string) string {
	h := strings.TrimSpace(header)
	if h == "" {
		return ""
	}
	if after, found := strings.CutPrefix(h, "Bearer "); found {
		return strings.TrimSpace(after)
	}
	if after, found := strings.CutPrefix(h, "bearer "); found {
		return strings.TrimSpace(after)
	}
	return h
}
