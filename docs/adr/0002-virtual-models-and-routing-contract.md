# ADR-0002 — Virtual models carry routing intent through the `model` field

**Status:** Accepted — **amended by [ADR-0007](0007-requested-model-as-baseline.md)**
**Date:** 2026-08-04

> **Amendment.** This ADR states that an explicitly requested model is an absolute pin. ADR-0007 narrows that: under opt-in optimization mode a requested model becomes a **baseline and ceiling**, and Relay may serve something cheaper that clears the quality floor, disclosing the swap on every affected response. The underlying rule — *never substitute silently* — is unchanged, and `X-Relay-Pin: strict` restores the behavior described below. Read this ADR for the contract; read ADR-0007 for how consented substitution fits inside it.

## Context

Relay must be usable from unmodified OpenAI SDKs. That is most of its value: teams adopt it by changing a base URL, not by rewriting call sites. But the OpenAI request schema is fixed, and it offers no field for "route this intelligently." Adding a custom top-level field breaks strict clients and gets stripped by some SDKs.

Meanwhile the system needs to express several distinct caller intents: *use exactly this model*, *use whatever is best for cheap coding work*, and *figure it out*. Without an explicit contract these blur together, and the most likely accident is the worst one — a caller pins a model, some policy or classifier disagrees, and they silently receive a different model's output while believing otherwise.

## Decision

Routing intent travels in the `model` field, which every SDK already exposes and never mangles. A `model` value resolves as one of:

1. **A real model or alias** (`gpt-4o-mini`) — pinned to a specific endpoint. No scoring, no classification.
2. **A virtual model** (`relay/fast-coder`) — names a **route**: a candidate endpoint list plus scoring weights and constraints.
3. **Auto** (`relay/auto`) — a route that also requests classification to select its weights.

`auto` is not a special mode. It is one route among many, which keeps the number of distinct code paths at one.

Precedence: explicit real model → named virtual model → tenant default route. Tenant policy filters candidates at every level.

**Policy may reject, but never substitute.** If policy forbids the model a caller explicitly pinned, the request fails with `403` naming the policy. It does not quietly answer from something else.

Relay-only options (session key, dry-run, weight overrides) travel as `X-Relay-*` headers or in `extra_body`, never as new top-level schema fields.

## Consequences

**Good.** Full OpenAI compatibility with zero client changes. Routing behavior is named, versioned config that can be reviewed, diffed, and rolled back. Every route is independently testable. Ops can change which models serve `relay/fast-coder` without touching an application. The explainability story is coherent because there is exactly one decision point.

**Costs.** Callers must learn the virtual model names — a documentation and discovery burden, partly mitigated by listing routes in `GET /v1/models`. Route names become a public interface that is awkward to rename once applications depend on them; they should be chosen for stability, and aliases used for renames.

**On the substitution rule.** Refusing to substitute means some requests fail that could technically have been served. That is the intended trade. A caller who asked for one model and silently received another has been given false information about their own system, and every conclusion they draw from that response — evaluations, cost attribution, incident analysis — is unsound. A visible failure is recoverable; a silent substitution is not.

## Alternatives considered

**A custom top-level field** (`"routing": {...}`). Rejected: breaks strict OpenAI clients and is dropped by several SDKs.

**Routing headers only, no virtual models.** Rejected: headers are awkward to set through high-level SDK wrappers and frameworks, and it leaves `model` meaningless in auto mode — which breaks logging, evaluation tooling, and anything that groups by model.

**Always classify, ignore what the caller sent.** Rejected outright. It makes the pinned case impossible, and pinning is a hard requirement for evaluation, reproducibility, and debugging.
