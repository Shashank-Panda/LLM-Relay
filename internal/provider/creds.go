package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/Shashank-Panda/relay/internal/domain"
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
	// Resolve returns the credential to use for this endpoint on behalf of this
	// tenant.
	//
	// ctx is here because a hosted deployment resolves against a credential
	// store, and a request that has been cancelled should not go on waiting for
	// a database. The environment-backed resolvers ignore it.
	//
	// The endpoint rather than the bare ref: a stored credential may legitimately
	// be scoped per deployment or per region, and a ref alone loses both. Callers
	// must not read anything else off it — nothing above the adapter layer is
	// allowed to branch on vendor name, and a resolver is above it.
	Resolve(ctx context.Context, tenant string, ep *domain.ModelEndpoint) (Credential, error)
}

// Availability answers "could you resolve this ref" without resolving it.
//
// Separate from Resolver because the router asks a different question than the
// executor does, and must not be handed a secret to ask it. The candidate set
// is decided from refs alone (domain.CredentialSet), so nothing above the
// executor ever holds key material — a property that is easy to keep now and
// impossible to recover once lost.
//
// A resolver that does not implement this is treated as "has everything", which
// is the fail-open direction: an unknown availability must not eliminate
// candidates (ADR-0010).
type Availability interface {
	Available(tenant, ref string) bool
}

// AvailableRefs returns the subset of refs the resolver can supply.
//
// Resolvers that cannot answer cheaply are asked the expensive way, once per
// distinct ref rather than once per candidate — a catalog has a handful of refs
// and dozens of endpoints.
func AvailableRefs(ctx context.Context, r Resolver, tenant string, refs []string) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if a, ok := r.(Availability); ok {
			if a.Available(tenant, ref) {
				out = append(out, ref)
			}
			continue
		}
		// A resolver that cannot answer cheaply is asked the expensive way. It
		// still yields a credential, which is why this function returns refs and
		// never the value: nothing above the executor should be holding one.
		if _, err := r.Resolve(ctx, tenant, &domain.ModelEndpoint{CredentialRef: ref}); err == nil {
			out = append(out, ref)
		}
	}
	return out
}

// ErrNoCredential is returned when a ref has no configured secret. Distinct
// from a wrong secret: this one is a deployment mistake, and the message should
// say so rather than reporting a 401 the operator will chase into a provider
// dashboard.
type ErrNoCredential struct {
	Ref    string
	EnvVar string

	// Hint tells the caller how to supply this credential *here*.
	//
	// The env-var form is right for a self-hosted install and wrong for a
	// hosted one, where it instructs somebody to set a variable on a machine
	// they do not own. Each resolver states its own remedy rather than the
	// message assuming a deployment shape.
	Hint string
}

func (e *ErrNoCredential) Error() string {
	hint := e.Hint
	if hint == "" && e.EnvVar != "" {
		hint = "set " + e.EnvVar
	}
	if hint == "" {
		return fmt.Sprintf("no credential configured for %q", e.Ref)
	}
	return fmt.Sprintf("no credential configured for %q (%s)", e.Ref, hint)
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

// Available reports whether this ref has a secret, without reading one into a
// caller's hands.
func (r *EnvResolver) Available(_, ref string) bool {
	if ref == "" {
		return false
	}
	if r.Free[ref] {
		return true
	}
	r.mu.RLock()
	_, cached := r.cached[ref]
	r.mu.RUnlock()
	return cached || os.Getenv(r.EnvVarFor(ref)) != ""
}

func (r *EnvResolver) Resolve(_ context.Context, _ string, ep *domain.ModelEndpoint) (Credential, error) {
	ref := ""
	if ep != nil {
		ref = ep.CredentialRef
	}
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
		return Credential{}, &ErrNoCredential{Ref: ref, EnvVar: env, Hint: "set " + env}
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

// Available reports whether this ref is in the map.
func (s StaticResolver) Available(_, ref string) bool {
	_, ok := s[ref]
	return ok
}

func (s StaticResolver) Resolve(_ context.Context, _ string, ep *domain.ModelEndpoint) (Credential, error) {
	ref := ""
	if ep != nil {
		ref = ep.CredentialRef
	}
	c, ok := s[ref]
	if !ok {
		return Credential{}, &ErrNoCredential{Ref: ref, EnvVar: "(static resolver)"}
	}
	return c, nil
}
