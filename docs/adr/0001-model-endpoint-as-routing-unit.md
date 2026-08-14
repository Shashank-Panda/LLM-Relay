# ADR-0001 — The routing unit is a model endpoint, not a provider

**Status:** Accepted
**Date:** 2026-08-04

## Context

The natural way to describe this system is "it routes between OpenAI, Anthropic, Gemini and Ollama." That framing is intuitive, it matches how people talk about model vendors, and it is wrong in a way that contaminates every component built on top of it.

"Route to Anthropic" is not a decision. Anthropic sells models that differ by roughly an order of magnitude in both price and latency, and that differ in context window, modality support, and quality. The same is true of every other vendor. A router that selects a provider has selected almost nothing, and then has to make the actual decision somewhere else — which means a second, undocumented routing step appears inside the adapter layer, exactly where provider-specific logic was supposed to be prohibited.

The error also propagates downward. If health, circuit breaking, and latency statistics are tracked per provider, then one overloaded large model trips the breaker for the vendor's cheap fast model too. Rate limits are worse: they are enforced per API key, so per-provider accounting is wrong even for a single vendor with two keys.

## Decision

The routing unit is a **model endpoint**, identified by the tuple `(provider, model, deployment, credential)` and given a stable string ID such as `anthropic/claude-sonnet-5@us-east`.

Consequently:

- The catalog is a set of endpoints. Providers are a field on an endpoint, not a first-class routable entity.
- Capabilities, limits, pricing, quality scores, and lifecycle attach to endpoints.
- Circuit breakers, latency EWMA, in-flight counts, and health attach to `(endpoint, credential)`.
- Rate limits attach to the credential.
- Adapters are per provider — they are the code that speaks a wire protocol. Endpoints are per callable model. One adapter serves many endpoints.

## Consequences

**Good.** Routing decisions are meaningful. Cost and latency scoring operate on real numbers rather than vendor averages. A degraded large model does not take down its sibling. Multi-region and multi-key deployments of the same model are expressible without special cases, which also makes Azure OpenAI deployments and self-hosted instances fall out naturally rather than needing a carve-out.

**Costs.** The catalog is larger and needs real maintenance — every model, every region, every price. Configuration is more verbose than a list of four vendor names. Metric cardinality is higher, bounded by endpoint count rather than provider count, which is acceptable because the endpoint set is operator-controlled and small.

**Retained.** Provider-level operations still exist where they genuinely are provider-level: adapter selection, a global provider disable switch, and grouping in dashboards. These are conveniences over the endpoint set, not the routing model.

## Alternatives considered

**Route to providers, choose the model inside the adapter.** Rejected: it relocates the real decision into the layer that is explicitly forbidden from containing routing logic, and makes that decision untestable and unexplainable.

**Route to `(provider, model)` without deployment or credential.** Rejected: it cannot express the same model in two regions or under two keys, which is a requirement for both data residency and rate-limit headroom — the two most common reasons a production deployment needs a second copy of a model it already has.
