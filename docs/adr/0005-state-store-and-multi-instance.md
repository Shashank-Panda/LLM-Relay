# ADR-0005 — Where state lives, and what is allowed to be per-process

**Status:** Accepted
**Date:** 2026-08-04

## Context

The original design named Redis as the only storage component. That is insufficient for something described as a control plane: budgets, audit logs, usage records, and API keys are systems-of-record data, and Redis is a cache.

The harder question is what happens under horizontal scaling. Several pieces of state — circuit-breaker status, latency EWMA, rate-limit counters, budget consumption — are naturally per-process, and behind N replicas each behaves differently. Some of those differences are harmless. One of them is a financial bug.

## Decision

### Storage tiers

| State | Store | Rationale |
|---|---|---|
| Tenants, API keys, budgets, usage records, audit log, decision history | **Postgres** | System of record. Durable, transactional, queryable. |
| Catalog, routes, policies | **Postgres**, snapshotted to memory | Written rarely via admin API; read on every request from an in-memory snapshot. |
| Rate-limit counters, budget reservations, response cache | **Redis** | Shared across instances; loss is tolerable and self-healing. |
| Latency EWMA, circuit-breaker state, in-flight counts | **Per-process** | Hot-path reads must not cross the network. |

### The rule that decides the tier

**Anything whose per-instance divergence costs money must be shared. Anything else may be local.**

Budgets and rate limits fail this test badly. A $100/day budget enforced per process, behind 10 replicas, is a $1,000/day budget — and the error is silent, discovered on an invoice. These live in Redis with atomic operations.

Budget enforcement additionally uses a **reservation model**: reserve an estimated maximum cost before the call, reconcile against actual usage after it completes, release the remainder. A simple check-then-spend has an obvious race — under concurrency every in-flight request reads the same "under budget" state and they all proceed, so the overshoot scales with concurrency exactly when the budget matters most.

Circuit-breaker state and latency statistics pass the test and stay local.

### Accepted divergence, stated plainly

With per-process breakers behind N replicas, each instance must independently observe an endpoint failing before it stops routing there. An outage is therefore detected up to N times, and the effective error threshold is per-instance rather than global. The cost is a bounded number of extra failed requests per instance per incident. The alternative — a Redis round trip on the hot path of every request to save those calls — costs more latency continuously than it saves in failures occasionally.

This is a deliberate trade, not an oversight, and it is revisited if `relay_attempts_total{error_class="RetryOther"}` shows the extra failures mattering at scale.

### Control plane failure must not stop inference

The data plane serves from its last good in-memory snapshot. If Postgres is unavailable: routing continues, requests are served, usage records buffer in memory and spill to disk, and config reloads simply do not happen. What degrades is budget enforcement precision and durable metering — not availability.

This is a hard requirement. If the database being down stops inference, the data-plane/control-plane separation has failed and there is no point having built it.

## Consequences

**Good.** Correct budget enforcement under concurrency and horizontal scale. Real queryable analytics and audit. The hot path takes zero network round trips for routing decisions. Data-plane instances are stateless and can be restarted, scaled, and rolled freely.

**Costs.** Two data stores to operate and back up, plus migrations. Reservation logic is more complex than a counter check and needs its own tests — the concurrency test in Phase 5 exists specifically for this. Redis becomes a dependency for rate limiting and budgets, so its failure mode needs a decision: **fail open** for rate limits (serve the request, log the gap) and **fail closed** for budgets (reject, because the alternative is unbounded spend).

## Alternatives considered

**Redis only.** Rejected: no durable audit or usage history, no real query capability, and no transactional guarantees for billing data.

**Postgres only.** Rejected: rate-limit counters and reservations at request rate are the wrong workload for a relational store, and add hot-path latency that Redis does not.

**Fully shared state including breakers and latency.** Rejected: a network round trip on every routing decision to make a rarely-consulted signal marginally fresher. It would make the p50 overhead SLO unreachable.
