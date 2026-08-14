# ADR-0009 — Quality is a hard floor, protected by sequential escalation

**Status:** Accepted
**Date:** 2026-08-04

## Context

Once Relay is permitted to serve a cheaper model than the one requested ([ADR-0007](0007-requested-model-as-baseline.md)), the dominant risk stops being cost and becomes quality. A downgrade that produces a worse answer does not save the customer money — it moves the cost from their invoice to their users, their retry logic, and their engineers' afternoons. That trade is worse than not optimizing at all, and it is invisible on the dashboard that shows savings going up.

The original architecture treated quality as a **scoring weight**: a dimension in the weighted sum, tradeable against cost. That is wrong for a commercial cost-reduction product, for a structural reason. Scores are normalized across the surviving candidate set, so quality is always tradeable *relative to whatever else is available*. Filter enough candidates out — a circuit opens, a policy denies, a region is required — and the remaining set can be uniformly poor while one of them still scores highest. Weighted quality has no floor; it only has a ranking.

The second problem is that catalog quality scores are **operator assertions**. Fine for a self-hosted gateway whose operator owns the consequences. Not a sufficient basis for telling a paying customer their output will not degrade.

## Decision

**Quality is a hard filter.** `Policy.QualityFloor` is a per-tenant, optionally per-route minimum on the relevant dimension. Candidates below it are eliminated in phase 1 with `BelowQualityFloor`, never merely down-ranked. The floor is the customer's explicit dial: it is where "how much cheaper" gets traded against "how much worse" in the open, rather than emerging from weights nobody reads.

**Cascade escalation is the backstop.** When a downgraded endpoint returns output that fails a cheap, objective validity check — empty completion, unparseable JSON, schema violation, malformed or undeclared tool call, refusal — the Executor retries once on the baseline endpoint.

- **Sequential, not parallel.** The second call only happens on actual failure. This is the distinction from hedging, which pays double on every request and is rejected in [architecture §6](../architecture.md#6-reliability).
- **One escalation, to the baseline.** No chains.
- **Negative savings are recorded.** All attempts are costed against one baseline, so a failed downgrade appears in the ledger as a loss. A savings number that excluded its own failures would be marketing, not measurement.
- **Escalation rate is the control signal.** Sustained escalation revises an endpoint's effective quality downward, so routing corrects itself without human intervention. SLO: under 2% per route.

**Asserted quality is replaced by measured quality** over time: offline evals per task type, then online signals that need no labelling (client retry rate, regeneration rate, tool-call error rate, schema violation rate), then per-tenant calibration — because aggregate quality is a weak predictor for any specific workload.

## Consequences

**Good.** A stated, auditable quality guarantee instead of an implicit one. The customer controls the floor. Escalation converts a class of silent quality failures into a visible, self-correcting cost. Escalation rate is a leading indicator of regression that fires before a customer complains.

**Costs.** Fewer savings than an unconstrained optimizer would claim — which is the point, and it should be said plainly in sales material rather than discovered later. Escalated requests cost more than not optimizing at all. Eval infrastructure is real work, and per-tenant calibration needs enough traffic to be meaningful, so early tenants run on global scores.

**The limit that must not be oversold.** Validity checks detect *invalid* output, not *worse* output. A cheaper model that returns a well-formed, schema-valid, subtly inferior answer passes every check. Escalation is therefore a floor on correctness, not on quality — which is exactly why `QualityFloor` exists as a separate upstream mechanism and why measured quality scores matter more than the cascade does.

**Streaming coverage is partial.** Checks needing the full response cannot run before the first-byte flush ([ADR-0003](0003-streaming-failover-semantics.md)). Streaming routes get first-chunk-decidable checks only. Stated here so it is not discovered as a surprise.

## Alternatives considered

**Quality as a weight, with a high coefficient.** Rejected: no floor, only a ranking. Relative normalization means a sufficiently depleted candidate set produces a bad choice that scores well.

**LLM-as-judge on every response.** Rejected: adds an inference to every request, which is the cost problem the product exists to solve, plus latency the SLO cannot absorb. Viable offline on sampled traffic for eval purposes; not in the hot path.

**No escalation — trust the floor.** Rejected: the floor rests on quality scores that are asserted rather than measured. Escalation is what makes an over-optimistic score self-correcting instead of silently harmful.
