# ADR-0006 — The classifier is a scorer input, not a pipeline stage

**Status:** Accepted
**Date:** 2026-08-04

## Context

The original design placed prompt classification and prompt analysis as mandatory synchronous stages in front of routing, for every request, with a stated evolution toward an AI-based classifier.

Three problems, in increasing severity.

**Latency.** A gateway's own overhead budget is single-digit milliseconds — that is the whole promise of putting one in front of your models. An LLM-based classifier adds a full model round trip, typically 200–800 ms, to every request. That is one to two orders of magnitude over the budget, and it is spent before the request that the user actually cares about has even started.

**Cost.** Classifying every request with a model means paying for an extra inference on every request. Routing must then save more than that on average just to break even. For requests where the cheap and expensive candidates differ by cents, it will not.

**Correctness.** Classification of a single prompt is a tractable problem. Classification of a 30-message conversation with a system prompt, interleaved tool results, and attached images is a much harder one, and it is the shape most real traffic takes. Worse, classifying a request whose caller already specified exactly what they wanted is not just wasted — it is an invitation to override them.

## Decision

Classification is **one optional input to the scorer**, not a stage in the request pipeline.

- It runs **only for `relay/auto` routes**. Never for a pinned model, never for an explicitly named route — in both cases the caller has already decided.
- It **selects weights and constraints**. It does not select an endpoint. The scorer selects the endpoint, always, from every input available to it.
- It is **deadline-bounded**, default 50 ms. On timeout, the route's default weights apply and the request proceeds. Classification may never be the reason a request is slow.
- It is **cached** by hash of the classification-relevant prefix, so a multi-turn conversation classifies once rather than every turn.
- **Low confidence falls back** to the route's default weights. A guess below the confidence threshold is discarded rather than acted on, because routing with false precision is worse than routing with none.

It is also **built last** — Phase 7 of the [roadmap](../roadmap.md), after transport, routing, reliability, observability, tenancy, and caching.

## Consequences

**Good.** The p50 overhead SLO stays achievable, because the common paths (pinned model, named route) do no classification work at all. Classification can be disabled entirely and the system still routes — it is an enhancement, not a dependency, and that is verifiable by turning it off. Building it last means it is designed against a scorer that already exists and whose real inputs are known, rather than against a guess.

**Costs.** `relay/auto` is less capable in early phases than the original vision implied — it routes on request-shape signals (token count, tool presence, modality) rather than semantic task type until Phase 7. Rule-based classification is less accurate than a model would be. Both are acceptable, because a mediocre classifier feeding a good scorer produces mediocre weight selection, whereas a good classifier feeding no scorer produces nothing.

**On building it last.** This is the part most likely to be argued with, since classification is the project's distinguishing idea. The argument for deferring it: it is the component whose requirements are least knowable in advance, and the one most likely to be redesigned once the scorer, the catalog, and real traffic data exist. Building it first means building it twice. Nothing else in the system depends on it, so there is no ordering constraint forcing it earlier — only enthusiasm.

## Alternatives considered

**Synchronous LLM classification on every request.** Rejected on latency and cost, as above.

**Asynchronous classification, applied to the next request in the session.** Interesting — it removes classification from the hot path entirely by always being one turn behind. Rejected for now because it makes the first turn of every conversation unclassified, and the first turn is often the only turn. Worth revisiting if the cached-classification hit rate proves low in practice.

**Classification as a hard filter** — for example, "this is a coding task, therefore only coding models are eligible." Rejected: it converts a probabilistic signal into a hard constraint, so a misclassification becomes an outright failure rather than a slightly worse ranking. Soft inputs belong in scoring, not filtering.
