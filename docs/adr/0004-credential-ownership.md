# ADR-0004 — Who owns provider API keys

**Status:** **Accepted (A) for the credential *transport*; storage remains open**
**Date:** 2026-08-04, amended 2026-08-19

> **Update.** Relay is now positioned as a hosted service with a self-hosted enterprise option. That does not close this ADR, but it narrows it sharply: as a vendor, holding pooled provider keys would mean fronting customers' model spend, carrying the float, and absorbing the fraud and abuse exposure of strangers' traffic — for a product whose margin is a gateway fee, not a markup on tokens. Option A (BYOK) is now the presumptive answer and Option B is close to excluded. It stays open because the migration path to C, and the handling of self-hosted installs where the distinction partly dissolves, are not yet settled.
>
> **Superseded 2026-08-19.** Both of those are now settled and the decision below is Option A for the credential *transport*. What remains open is storage: encrypted per-tenant credentials, rotation, and onboarding.

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
- A tenant with no key for an endpoint means that endpoint is filtered out with `NoCredential`, so different tenants see genuinely different candidate sets. Routing must handle this, and it already does. **[This last sentence was false when written; see the correction in the decision below. It is true as of 2026-08-19.]**

### B — Pooled: Relay owns the keys

Relay holds one set of provider credentials and meters usage back to tenants.

- Trivial onboarding — a tenant needs only a Relay API key.
- Volume pricing and shared prompt-cache benefits across tenants.
- Relay is now financially exposed. Budgets, quotas, and cost accounting stop being features and become load-bearing controls; a bug in budget enforcement is a bill.
- Rate limits are shared, so one tenant's spike degrades everyone. Requires per-tenant rate limiting on top of provider limits, and probably a credential pool with rotation across keys.
- Relay must reconcile its metered usage against provider invoices, or the numbers drift and nobody trusts them.

### C — Both, resolved per tenant

Tenant-supplied credentials when present, pooled fallback otherwise. Maximum flexibility, and the union of both implementations' complexity. Likely the eventual answer; a poor starting point.

## Decision (2026-08-19)

**Option A — BYOK — for how a credential reaches Relay.** A caller may supply provider keys on the
request, one header per credential ref:

    X-Relay-Credential: openai-primary sk-...

Resolution is a chain: the caller's own keys first, the deployment's environment second
(`provider.Chain{keySet, envResolver}`). That is the correct default for both deployments without a
flag — self-hosted, the operator's keys serve everyone and a caller may still override; hosted, the
environment holds nothing and the caller's key is the only source. It also makes "hosted is a
configuration change rather than a rewrite" literally true: the change is dropping the second link.

**What this decides and what it does not.** This settles the *transport* and the resolution order.
It does not decide encrypted per-tenant credential *storage*, rotation, or an onboarding flow —
those remain Phase 7 and remain genuinely open. A request-scoped key is used for the request that
carried it and then discarded; nothing stores one. Reading "BYOK is decided" as "credential storage
is decided" would be a mistake, which is why the status line above names only half.

**Option B is now excluded** for the hosted product, for the reason the 2026-08-04 update gave:
holding pooled provider keys means fronting customers' model spend and absorbing strangers' abuse
exposure, for a product whose margin is a gateway fee. **Option C is the destination** and arrives
almost for free — the chain above already resolves caller-supplied credentials ahead of a shared
fallback, so a pooled key becomes a third link if there is ever a reason to add one.

### The signature, as shipped

    type Resolver interface {
        Resolve(ctx context.Context, tenant string, ep *domain.ModelEndpoint) (Credential, error)
    }

Two deviations from the sketch below, both deliberate. It takes `tenant` as a `string` rather than a
`TenantID` type, which does not exist. And `ErrNoCredential` now carries a `Hint` naming the remedy
*for that resolver*, because the environment resolver's advice — "set `RELAY_CRED_X`" — instructs a
hosted caller to configure a machine they do not own.

### A correction to what this ADR previously claimed

The 2026-08-04 text asserted, of a tenant with no key for an endpoint:

> A tenant with no key for an endpoint means that endpoint is filtered out with `NoCredential`, so
> different tenants see genuinely different candidate sets. Routing must handle this, and it already
> does.

**That was false when written.** `RejectNoCredential` existed, but `routing/filter.go` only checked
whether the catalog *named* a `credential_ref` — it never asked a resolver whether that ref
resolved. Every endpoint in the catalog was ranked for every caller, so a caller holding one
provider's key would see endpoints they could not authenticate to ranked above the ones they could,
and would then pay for that ranking one `RetryOther` attempt at a time.

It is true now. `routing.Input` takes a `*domain.CredentialSet` — refs only, never secrets —
snapshotted outside so `Route` stays a pure function, and a nil set means "no opinion" and
eliminates nothing, which is the ADR-0010 direction.

### Two consequences that were not obvious

**The response cache's isolation unit had to change.** Entries were scoped by tenant, which is
correct while credentials come from the deployment's environment: everyone sharing a tenant is the
same customer. Under BYOK it fails, and severely — two strangers evaluating a hosted Relay are both
the anonymous default tenant and both bring their own keys, so an identical prompt is one cache key
and a hit, and one of them receives an answer generated on the other's credential together with
confirmation that a stranger asked that exact question. The scope is now the tenant plus, when the
caller supplied credentials, a truncated salted digest of the key material
(`gateway.Prepared.CacheScope`). `keySchema` moved to `v2`.

**A keyless explanation and credential filtering destroy each other.** Once routing eliminates
endpoints for want of a key, a dry run on a machine with no keys collapses from a full ranking to a
single row — and that ranking is the most persuasive thing this product can show someone who has
not adopted it yet. Resolved with `X-Relay-Assume-Credentials: all`, honoured **only** on a dry run
and ignored on every live request, plus a `credentials` block in the response reporting which refs
the caller actually holds. A header that could route live traffic to an endpoint Relay cannot
authenticate to would be a denial of service with a polite name.

## Original decision (interim, 2026-08-04)

Deferred. The design preserved the choice through a single seam:

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

## What would close the remainder

Storage is what is left. Answer these and the rest follows:

Answer these and the decision follows:

1. Is Relay internal infrastructure for one organization, or multi-organization?
2. Does anyone want Relay to appear on their invoice?
3. Is there an appetite for storing other people's provider credentials, with the security obligations that carries?

For a single organization, (B) with tenants as internal teams is clearly right. For anything multi-organization, (A) is the lower-risk start and (C) the destination.

## Consequences

Encrypted credential storage, key rotation, and the onboarding flow stay unbuilt until Phase 7,
which is where they are scheduled regardless. They arrive behind the interface above, as another
link in the chain, and change no call site.

The obligation taken on today is narrower and worth stating plainly: Relay now accepts other
people's secrets in flight. That is defended structurally rather than by care — `Credential` and
`KeySet` both redact under every format verb, `meter.Record` has no field a credential could occupy,
the access log renders no headers, and error messages name the header position rather than its
value. `TestCredentialNeverEscapes` asserts all four at once against a canary, because each is a
different way the same string escapes and each would go unnoticed differently.

A key sent over plaintext HTTP is refused unless `-allow-insecure-credentials` is set. Refused
rather than warned about: the request would otherwise succeed, so nothing anywhere would record that
a live key had crossed the network in the clear.
