# ADR-0004 — Who owns provider API keys

**Status:** **OPEN** — deliberately undecided
**Date:** 2026-08-04

## Context

Someone has to hold the OpenAI and Anthropic keys. Which someone is not yet decided, and the answer changes the shape of billing, secret handling, rate limiting, and the operational risk profile. It is recorded here as open rather than guessed at, because guessing wrong is expensive and the decision can be deferred cheaply behind an interface.

Nothing else in the documentation assumes an answer. If you find something that does, it is a bug in the docs.

## Options

### A — BYOK: each tenant supplies its own keys

Relay stores encrypted per-tenant provider credentials and uses the calling tenant's keys.

- Provider bills the tenant directly; Relay never fronts money and never becomes a billing intermediary.
- Rate limits are naturally isolated per tenant — one tenant cannot exhaust another's quota.
- Requires encrypted credential storage, a rotation story, and a validation flow at onboarding.
- Tenant onboarding is heavier; every tenant needs accounts with every provider they want to use.
- Cost tracking becomes advisory rather than authoritative — useful for visibility, not for billing.
- A tenant with no key for an endpoint means that endpoint is filtered out with `NoCredential`, so different tenants see genuinely different candidate sets. Routing must handle this, and it already does.

### B — Pooled: Relay owns the keys

Relay holds one set of provider credentials and meters usage back to tenants.

- Trivial onboarding — a tenant needs only a Relay API key.
- Volume pricing and shared prompt-cache benefits across tenants.
- Relay is now financially exposed. Budgets, quotas, and cost accounting stop being features and become load-bearing controls; a bug in budget enforcement is a bill.
- Rate limits are shared, so one tenant's spike degrades everyone. Requires per-tenant rate limiting on top of provider limits, and probably a credential pool with rotation across keys.
- Relay must reconcile its metered usage against provider invoices, or the numbers drift and nobody trusts them.

### C — Both, resolved per tenant

Tenant-supplied credentials when present, pooled fallback otherwise. Maximum flexibility, and the union of both implementations' complexity. Likely the eventual answer; a poor starting point.

## Decision (interim)

Deferred. The design preserves the choice through a single seam:

```go
type CredentialResolver interface {
    // Returns the credential to use for this endpoint on behalf of this tenant.
    // Returns ErrNoCredential when none is available, which the router turns
    // into a NoCredential rejection during the filter phase.
    Resolve(ctx context.Context, tenant TenantID, ep *ModelEndpoint) (Credential, error)
}
```

Every consequence of the decision is reachable from behind this interface:

- **Filtering.** `NoCredential` is already a first-class reject reason, so tenant-specific candidate sets work under any option.
- **Rate limits and circuit breakers** are keyed on `(endpoint, credential)` rather than on endpoint alone ([ADR-0001](0001-model-endpoint-as-routing-unit.md)), so per-tenant and pooled isolation both work without restructuring.
- **Cost records** store the credential reference, so usage can be attributed either way.
- **Budgets** are built with reservation semantics regardless, because they are needed for pooled and useful for BYOK.

Until this ADR is closed, Phase 1 uses a `StaticResolver` reading a single set of keys from environment variables — sufficient for development, and not a commitment.

## What would close this

Answer these and the decision follows:

1. Is Relay internal infrastructure for one organization, or multi-organization?
2. Does anyone want Relay to appear on their invoice?
3. Is there an appetite for storing other people's provider credentials, with the security obligations that carries?

For a single organization, (B) with tenants as internal teams is clearly right. For anything multi-organization, (A) is the lower-risk start and (C) the destination.

## Consequences of deferring

Encrypted credential storage, key rotation, and the onboarding flow stay unbuilt until Phase 5, which is where they are scheduled regardless. The cost of deferral is close to zero. The cost of choosing wrong now is a storage schema, a security model, and a billing pipeline built for the wrong shape.
