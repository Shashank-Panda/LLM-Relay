# ADR-0008 — Request optimization is a separate component from routing

**Status:** Accepted
**Date:** 2026-08-04

## Context

The original architecture optimized cost by one mechanism: choosing a cheaper endpoint. That is a large lever and it is not the only one, and for many workloads it is not even the biggest.

A request carries several sources of avoidable cost that have nothing to do with which model serves it:

- **Prompt caching left unactivated.** Providers charge a fraction of the input price to read a cached prefix, but only when breakpoints are placed at stable boundaries. Doing this correctly across every call site by hand is exactly the kind of work teams never get to.
- **Reasoning effort set globally.** Configured once at the highest setting during development because tuning it per call site was not worth anyone's afternoon.
- **`max_tokens` set to whatever the example used**, leaving an unbounded ceiling on output the caller does not need.
- **Stale context** — superseded tool results and old turns re-sent at full price every turn.

These are recoverable without changing which model answers, which means they carry **no quality-floor risk**. Cost reduction with no quality trade is strictly better than cost reduction with one, so it should be exhausted first.

## Decision

Introduce an **Optimizer** that runs after normalization and before routing:

```go
Apply(req *NormalizedRequest, pol *Policy, stats *RouteStats) (*NormalizedRequest, []Optimization)
```

Separate from the Router because it answers a different question — the Optimizer transforms *the request*, the Router selects *the endpoint*. Different inputs, different outputs, independently testable. Merging them would produce a component that does both jobs and can be unit-tested for neither.

Like the Router it is **pure and deterministic**: it reads a stats snapshot, performs no I/O, and returns the same output for the same input.

Three rules bound every lever:

1. **Never override an explicit caller value.** If the caller set `max_tokens` or a reasoning effort, that is a decision. Optimization fills in absent values and lowers unbounded ones; it does not contradict stated intent.
2. **Semantics-preserving by default.** Cache breakpoints, effort downshift, and output ceilings do not change what the model is asked. Context pruning does, so it is a separate opt-in.
3. **Every adjustment is recorded** in `Decision.Optimizations` with before/after values, visible in dry-run and in response headers. An optimization the customer cannot see is indistinguishable from a bug.

Cache breakpoint *placement* lives in the Optimizer because identifying a stable prefix is provider-neutral; breakpoint *encoding* stays in the adapter because the wire format is not.

## Consequences

**Good.** Meaningful savings on workloads where model substitution is unavailable or unwanted — including tenants running in `strict` mode, who now get a real benefit from Relay without ever being downgraded. Cache breakpoint insertion in particular is high-value and genuinely hard for customers to replicate. Each lever is independently switchable, so a customer can adopt the risk-free ones and decline the rest.

**Costs.** A new component on the hot path, bounded to a 3 ms p99 budget and failing open to passthrough when exceeded. Cache breakpoint placement is provider-specific in its effects and needs per-provider validation that it actually produces cache hits — an incorrectly placed breakpoint is silently useless, so `relay_cache_breakpoints_inserted_total` must be read against the provider's reported `cached_input_tokens`, never on its own.

**The subtle risk.** Output ceilings are the one lever that can truncate a legitimately long response. Mitigation: the ceiling derives from the route's own observed p95 output length, never from a global default, and truncation via `finish_reason: length` is counted and alerted. A ceiling that fires regularly is wrong and should raise itself.

## Alternatives considered

**Put optimization inside the Router.** Rejected: conflates two decisions with different inputs, and makes `Route` impure by giving it a second output.

**Put it in the adapters.** Rejected: every lever would be reimplemented per provider, and the levers are mostly provider-neutral in their logic.

**Ship model substitution only, add levers later.** Rejected on sequencing grounds. The zero-quality-risk savings are the ones a cautious customer accepts first, so they are what makes the initial sale — and they work in `strict` mode, which is the default.
