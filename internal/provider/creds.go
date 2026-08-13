package provider

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// Credential is what an adapter needs to authenticate one call.
//
// Opaque on purpose. Whether the value came from a tenant's own key, a pooled
// Relay key, or a secret manager is a question for the resolver, and ADR-0004
// has not answered it. Adapters must not care, and this type is what keeps them
// from being able to.
type Credential struct {
	// Ref is the catalog's CredentialRef, safe to log. It names the credential
	// without being one.
	Ref string

	// APIKey is the secret. Never logged, never placed in an error message,
	// never included in a Decision.
	APIKey string

	// Headers are extra provider-specific auth headers (Azure's api-key,
	// project or organization scoping). Values are as sensitive as APIKey.
	Headers map[string]string
}

// String redacts.
//
// This method is the entire reason Credential is a struct rather than a string.
// Anything that formats a Credential with %v or %s — a log line, an error, a
// panic dump — gets the reference and not the secret. Relying on every future
// call site to remember is not a security posture.
func (c Credential) String() string {
	if c.Ref == "" {
		return "credential(unset)"
	}
	return "credential(" + c.Ref + ")"
}

// GoString redacts under %#v too, which is what most debug printing uses.
func (c Credential) GoString() string { return c.String() }

// Resolver turns a catalog CredentialRef into a usable credential.
//
// This is the seam ADR-0004 is deliberately left open behind. BYOK-per-tenant
// resolves the ref against the tenant's stored keys; pooled resolves it against
// Relay's own; a self-hosted install resolves it against the environment. All
// three are this one method, and choosing between them later changes one
// implementation rather than every adapter.
type Resolver interface {
	Resolve(ref string) (Credential, error)
}

// ErrNoCredential is returned when a ref has no configured secret. Distinct
// from a wrong secret: this one is a deployment mistake, and the message should
// say so rather than reporting a 401 the operator will chase into a provider
// dashboard.
type ErrNoCredential struct {
	Ref    string
	EnvVar string
}

func (e *ErrNoCredential) Error() string {
	return fmt.Sprintf("no credential configured for %q (set %s)", e.Ref, e.EnvVar)
}

// EnvResolver reads credentials from the environment.
//
// The credential_ref "anthropic-primary" reads RELAY_CRED_ANTHROPIC_PRIMARY.
// Good enough for Phase 1 and for self-hosted installs; Phase 7 adds a
// Postgres-backed implementation of the same interface.
type EnvResolver struct {
	// Prefix defaults to "RELAY_CRED_".
	Prefix string

	// Free lists refs that legitimately have no secret — a local Ollama, say.
	// Without this, every endpoint would need a dummy key configured, and a
	// dummy key is indistinguishable from a real one that stopped working.
	Free map[string]bool

	mu     sync.RWMutex
	cached map[string]Credential
}

// EnvVarFor reports which variable holds a ref's secret, for error messages and
// for documentation that cannot drift from the code.
func (r *EnvResolver) EnvVarFor(ref string) string {
	prefix := r.Prefix
	if prefix == "" {
		prefix = "RELAY_CRED_"
	}
	name := strings.ToUpper(ref)
	name = strings.NewReplacer("-", "_", ".", "_", "/", "_", "@", "_").Replace(name)
	return prefix + name
}

func (r *EnvResolver) Resolve(ref string) (Credential, error) {
	if ref == "" {
		return Credential{}, &ErrNoCredential{Ref: ref, EnvVar: "(none)"}
	}

	r.mu.RLock()
	c, ok := r.cached[ref]
	r.mu.RUnlock()
	if ok {
		return c, nil
	}

	env := r.EnvVarFor(ref)
	key := os.Getenv(env)
	if key == "" && !r.Free[ref] {
		return Credential{}, &ErrNoCredential{Ref: ref, EnvVar: env}
	}

	c = Credential{Ref: ref, APIKey: key}

	r.mu.Lock()
	if r.cached == nil {
		r.cached = make(map[string]Credential)
	}
	r.cached[ref] = c
	r.mu.Unlock()

	return c, nil
}

// StaticResolver is a fixed map, for tests and for self-hosted installs that
// mount a secrets file.
type StaticResolver map[string]Credential

func (s StaticResolver) Resolve(ref string) (Credential, error) {
	c, ok := s[ref]
	if !ok {
		return Credential{}, &ErrNoCredential{Ref: ref, EnvVar: "(static resolver)"}
	}
	return c, nil
}
