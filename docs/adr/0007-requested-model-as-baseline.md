# ADR-0007 — The requested model is a baseline and a ceiling, not a pin

**Status:** Accepted — amends [ADR-0002](0002-virtual-models-and-routing-contract.md)
**Date:** 2026-08-04

## Context

[ADR-0002](0002-virtual-models-and-routing-contract.md) established that an explicitly requested model is a hard pin: Relay serves it, unscored. That rule exists to prevent the worst failure mode a gateway has — a caller asking for one model, silently receiving another, and drawing wrong conclusions about their own system from the result.

Relay is now positioned as a service that reduces a customer's model spend **without requiring them to change their code**. A customer swaps a base URL and keeps sending `model: "gpt-4o"`.

Under ADR-0002 as written, that customer saves nothing. Every request is a pin, so no optimization ever runs. The product's headline feature is disabled by design for exactly the customer it is sold to.

Both positions are defensible in isolation. The task is to get the product without giving up the property that makes it trustworthy.

## Decision

An explicitly requested model resolves to a **baseline**, carrying a mode:

| Mode | Behavior | Default |
|---|---|---|
| `strict` | Serve exactly what was requested. No optimization, no substitution. | ✔ |
| `shadow` | Serve exactly what was requested; route and price the counterfactual and record the saving that *would* have occurred. | |
| `optimize` | Serve the cheapest endpoint that clears the quality floor and does not exceed the baseline. | |

Five constraints make substitution consented rather than silent:

1. **Opt-in per tenant.** `strict` is the default. A tenant turns optimization on deliberately, having been told what it does.
2. **The baseline is a ceiling.** Relay never serves an endpoint more expensive than the one requested. It cannot upsell.
3. **The quality floor is a hard filter**, not a weight — see [ADR-0009](0009-quality-floor-and-cascade.md).
4. **Every substitution is disclosed** on the affected response via `X-Relay-Served`, `X-Relay-Baseline`, and `X-Relay-Saved-Usd`. Not sampled, not deferred to a report.
5. **`X-Relay-Pin: strict`** overrides everything, per request, unconditionally.

ADR-0002's rule survives intact in its actual form: **Relay never substitutes silently, and policy still may not substitute at all.** A policy denial fails with `403`; it does not reroute. What changed is that a tenant may now grant advance, revocable, per-request-escapable permission for bounded downgrade — and that permission is a different thing from a gateway deciding on its own.

## Consequences

**Good.** One-line adoption. The product claim becomes true. Shadow mode gives a zero-risk evaluation path — a customer can measure a month of hypothetical savings before changing any behavior, which is both the lowest-friction sale and the most honest one. `strict` remains available for evaluation harnesses, reproducibility work, and anyone who simply does not want this.

**Costs.** Two response-shaping paths instead of one. The disclosure headers become a public API contract that cannot be broken. Support load: some fraction of "the model gave a different answer than yesterday" tickets are now Relay's fault, and the headers exist partly so those tickets are answerable in one step.

**The risk being accepted.** A customer enables optimization, does not read the headers, and is surprised. Mitigations: `strict` default, shadow mode as the recommended first step, disclosure on every response rather than in aggregate, and escalation on validity failure. What remains is a customer who opted in and ignored the disclosure — a materially different situation from one who was never told.

## Alternatives considered

**Keep pins absolute; require `relay/auto`.** Rejected: it makes "no code changes" false, which is the whole pitch. It also biases adoption toward teams willing to do migration work — the opposite of the target customer.

**Optimize by default, opt out.** Rejected. Defaults are consent, and a default that silently changes which model serves production traffic is consent nobody gave. It also makes the first bad answer a breach of trust rather than a tuning problem.

**Disclose in the monthly report only.** Rejected: a swap the caller learns about weeks later is functionally silent at the moment it matters, which is while they are debugging an unexpected response.
