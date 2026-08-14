# ADR-0010 — Relay fails open to passthrough

**Status:** Accepted
**Date:** 2026-08-04

## Context

As a hosted service, Relay sits in the critical path of its customers' production traffic. Their availability becomes the product of theirs and Relay's. They are being asked to accept a new single point of failure in exchange for a cost reduction — and a cost reduction is worth far less than an outage costs.

This inverts the usual gateway posture. A self-hosted gateway can fail closed on internal errors, because the operator owns both sides and can reason about the trade. A vendor in someone else's request path cannot: an error Relay generates is an error the customer did not have before adopting it.

The observation that resolves it: **almost everything Relay does is optional.** Optimization, routing, classification, metering, savings accounting — none of these are required to return a model response. Exactly one thing is mandatory: forwarding the request to a provider and streaming the answer back.

## Decision

Every internal failure degrades toward passthrough. The only errors that may surface to the caller are failures of the provider call itself.

| Failure | Behavior |
|---|---|
| Optimizer panics or exceeds its 3 ms budget | Route the unmodified request |
| Classification unavailable or slow | Route with default weights |
| Catalog snapshot stale or unloadable | Serve from the last good snapshot |
| Control plane / Postgres unreachable | Serve; buffer usage records, spill to disk |
| Redis unreachable | Rate limits fail **open**; budgets fail **closed** |
| Routing produces no viable candidate | Serve the baseline endpoint directly |
| Savings ledger write fails | Serve; the record is lost, the request is not |

**Two deliberate exceptions**, stated so that "fail open" is not read as "fail open always":

- **Budgets fail closed.** Failing open on a spend control means unbounded spend, which is worse than a failed request and is not recoverable after the fact.
- **Policy denials fail closed.** A policy that stops applying under load is not a policy. Data residency and provider restrictions exist precisely for the conditions under which someone would be tempted to relax them.

**Every degradation is counted.** `relay_degraded_total{component}` increments on each fail-open path. Passthrough is silent by design — the customer sees a normal response — so without this metric Relay can be optimizing nothing, saving nothing, and still look perfectly healthy on every other dashboard.

The availability SLO rises to **99.95%** monthly on the back of this, and the target is only reachable *because* of it: most Relay failures should degrade to "expensive but working," not to downtime.

## Consequences

**Good.** Relay is a safe dependency, which is a prerequisite for anyone putting it in front of production traffic. The failure mode is a lost saving, not a lost request. It also makes the deployment story honest — "worst case, you pay what you pay today" is a claim a buyer can evaluate.

**Costs.** Failures are quiet. A misconfigured Optimizer or a stale catalog produces correct responses at full price indefinitely, and nobody notices unless the degradation metric is actually alerted on — so that alert is not optional, and neither is a savings-rate anomaly alert. Testing is harder: every component needs a "what happens when this breaks mid-request" test, and passthrough paths need coverage precisely because they run rarely.

**A stale catalog is the sharpest edge.** Serving from the last good snapshot means a retired model can keep being selected, or a price change can go unapplied and quietly skew every savings number computed against it. Snapshot age is therefore a monitored value with its own alert, and a snapshot beyond its maximum age degrades further — to baseline passthrough rather than to routing on stale data. Old data is more dangerous than no data, because the system stays confident.

## Alternatives considered

**Fail closed on internal errors.** Rejected: makes Relay's bugs into the customer's outages, for a feature that is by nature optional.

**Fail open on budgets too, for consistency.** Rejected: the failure is unbounded spend, and it is discovered on an invoice. Inconsistency here is correct and worth documenting rather than smoothing over.

**A client-side SDK that bypasses Relay on failure.** Rejected: requires the code change the product exists to avoid, and moves failover logic into every customer's application where it cannot be fixed or observed.
