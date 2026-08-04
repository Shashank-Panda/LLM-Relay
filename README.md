# Relay

> An AI gateway that puts one OpenAI-compatible API in front of many LLM providers, and decides which model should answer each request.

Relay sits between your applications and every model vendor you use. Applications talk to it with the OpenAI SDK they already have; Relay normalizes the request, picks a model endpoint according to policy, executes the call with retries and failover, and streams the answer back in a single consistent format. Every decision it makes is recorded and explainable.

---

## Status

**Pre-implementation.** This repository currently contains design documentation only. No code has been written yet.

The architecture below is settled enough to build against. The [roadmap](docs/roadmap.md) describes what gets built in what order, and [`docs/adr/`](docs/adr/) records the decisions — including the ones still open.

---

## Why this exists

Teams accumulate model vendors. Each one has its own SDK, its own streaming format, its own tool-calling schema, its own failure modes, and its own bill. The usual result is provider-specific code scattered across services, no consolidated view of spend, and no way to switch models without a deploy.

Relay centralizes that. One endpoint, one request format, one set of metrics, one place where "which model handles this kind of work" is a configuration decision rather than a code change.

---

## Architecture at a glance

Relay separates a stateless **data plane** that serves requests from a **control plane** that holds configuration and accounting. The data plane is the hot path and is designed to add single-digit milliseconds; the control plane is where tenants, budgets, credentials, and the model catalog live.

```
                         ┌──────────────────────────────────┐
   OpenAI-compatible     │           DATA PLANE             │
   clients ─────────────▶│                                  │
   (any SDK, any lang)   │  auth ▸ limits ▸ validate ▸       │
                         │  normalize                       │
                         │            │                     │
                         │            ▼                     │
                         │  ┌──────────────────┐            │
                         │  │ Router  (pure)   │◀── catalog │
                         │  │ filter▸score▸rank│◀── policy  │
                         │  └────────┬─────────┘◀── health  │
                         │           │ Decision             │
                         │           ▼                      │
                         │  ┌──────────────────┐            │
                         │  │ Executor         │            │
                         │  │ retry / failover │            │
                         │  │ circuit breaker  │            │
                         │  └────────┬─────────┘            │
                         │           │                      │
                         │  ┌────────▼─────────┐            │
                         │  │ Provider adapters│            │
                         │  │ OpenAI Anthropic │            │
                         │  │ Gemini  Ollama   │            │
                         │  └────────┬─────────┘            │
                         │           │                      │
                         │   normalize ▸ stream ▸ meter     │
                         └───────────┬──────────────────────┘
                                     │ usage + decision records
                         ┌───────────▼──────────────────────┐
                         │          CONTROL PLANE           │
                         │  tenants · API keys · budgets    │
                         │  model catalog · routing policy  │
                         │  provider credentials · audit    │
                         │  admin API · analytics           │
                         └──────────────────────────────────┘
```

Two properties of this shape matter more than the boxes:

**Routing is a pure function.** `Router.Route(request, catalog, policy, health) → Decision` performs no I/O. It is a deterministic transformation of inputs to a ranked list with reasons attached, which makes it exhaustively testable and makes the explainability API free — the `Decision` *is* the explanation.

**Execution is separate from routing.** The `Executor` takes a `Decision` and carries it out, handling retries, failover between candidates, and streaming. Routing never knows about HTTP; execution never re-derives preferences.

See [`docs/architecture.md`](docs/architecture.md) for the full picture.

---

## How routing works

Clients select behavior through the one field every OpenAI SDK exposes — `model`:

```jsonc
{ "model": "gpt-4o-mini",        // a real model: pinned, routed straight through
  "messages": [...] }

{ "model": "relay/fast-coder",   // a virtual model: a named route with candidates + policy
  "messages": [...] }

{ "model": "relay/auto",         // classifier picks the task type, scorer picks the endpoint
  "messages": [...] }
```

A virtual model names a **route**: a candidate list of model endpoints plus the weights used to score them. Routing then runs in two distinct phases — **filter** on hard constraints (context window, required capabilities, policy, credential availability, circuit state), then **score** the survivors on normalized, weighted preferences (cost, latency, quality, prompt-cache affinity).

The routing unit is a *model endpoint* — `(provider, model, deployment, credential)` — not a provider. Routing "to Anthropic" is not a decision when Opus and Haiku differ by an order of magnitude in both cost and latency.

Full details, including a worked scoring example, are in [`docs/routing.md`](docs/routing.md).

---

## Documentation

| Document | Contents |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | Component design, core types, streaming, reliability, state, SLOs |
| [`docs/routing.md`](docs/routing.md) | Virtual models, the routing contract, scoring, the catalog, explainability |
| [`docs/roadmap.md`](docs/roadmap.md) | Build order, and what is explicitly out of scope |
| [`docs/adr/`](docs/adr/) | Architecture decision records, including open decisions |

---

## Stack

Go · Chi · Viper · Zap · Postgres · Redis · Prometheus · OpenTelemetry · Docker

---

## Design principles

- **Provider logic lives only in adapters.** Nothing above the adapter layer may branch on vendor name.
- **Decisions are data.** Routing produces a struct, not a side effect.
- **Configuration over code.** Adding a model, changing a route, or shifting cost weights is a config change.
- **Interface-driven, with small interfaces.** A provider adapter should be implementable in an afternoon.
- **The hot path stays cheap.** Anything that isn't required to answer the request happens off it.

---

## License

Not yet chosen.
