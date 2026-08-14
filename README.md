# Relay

> **Cut your LLM bill without changing your code.** Point your OpenAI SDK at Relay, keep calling the models you already call, and Relay serves each request from the cheapest option that still meets your quality bar — then shows you exactly what it saved.

Relay is an AI gateway that sits between your applications and your model providers. Applications talk to it with the OpenAI SDK they already have, sending the model names they already send. Relay normalizes the request, optimizes it, routes it to the cheapest endpoint that clears the quality floor, and streams the answer back unchanged in shape.

The two things that make it a product rather than a proxy: **it never substitutes silently** — every swap is disclosed in response headers with the saving attached — and **it measures the counterfactual**, so "we cut your spend 38%" is a number you can audit rather than a claim you have to believe.

---

## Status

**Phases 1–2 built, phases 3–8 not started.** Relay serves `/v1/chat/completions` (streaming and non-streaming), `/v1/models`, `/healthz`, and `/readyz` against Ollama, OpenAI, and Anthropic, and records a per-request savings ledger.

`strict` and `shadow` modes work, per tenant. **Nothing is ever substituted yet** — `optimize` mode is [Phase 5](docs/roadmap.md). Shadow mode serves exactly what was asked and records what a cheaper route *would* have cost on the same tokens, which is the whole point: a customer can measure a month of savings before granting permission to change anything.

```sh
go test ./...
export RELAY_CRED_ANTHROPIC_PRIMARY=sk-ant-...    # or RELAY_CRED_OPENAI_PRIMARY
go run ./cmd/relay                                # data plane :8080, admin 127.0.0.1:9090

curl -s localhost:9090/savings?tenant=acme        # the savings report
curl -s localhost:9090/metrics                    # Prometheus
```

**`saved` and `shadow_saved` are different numbers and are never summed.** In shadow mode the baseline is served, so nothing is actually saved; `shadow_saved` is what optimization *would* have saved. That separation runs from the ledger through the metrics to the report, because reporting the second as the first would tell a customer they had banked money they had not.

Prices in `config/catalog.yaml` are illustrative. Re-verify them against each provider's pricing page before pointing this at real traffic — the loader refuses an attestation older than 90 days. Tenant API keys live in `config/tenants.yaml` as SHA-256 digests; a missing file means every request is anonymous, which is what Phase 1 did.

The [roadmap](docs/roadmap.md) describes what gets built in what order; [`docs/adr/`](docs/adr/) records the decisions, including the ones still open.

---

## The problem

A team standardizes on a strong model because it was the safe choice during development. Then production traffic arrives, and most of it turns out to be easy — short classifications, formatting, extraction, routine tool calls — all being served by a frontier model at frontier prices. Meanwhile the reasoning budget is set high globally because nobody wanted to tune it per call site, prompt caching is left on the table because inserting breakpoints correctly is fiddly, and `max_tokens` is set to whatever the example used.

Every one of those is a real, recoverable cost. None of them get fixed, because fixing them means auditing hundreds of call sites, and nobody can prove in advance that the cheaper option is good enough.

Relay makes those decisions per request, at the gateway, with the evidence attached.

---

## Where the savings come from

Model substitution is only part of it. Relay pulls four levers:

| Lever | What it does | Typical saving |
|---|---|---|
| **Model downgrade** | Serve an easy request from a cheaper endpoint that clears the quality floor | Large, highly traffic-dependent |
| **Prompt-cache activation** | Insert cache breakpoints automatically, and keep sessions on the endpoint holding the warm cache | Up to ~90% of repeated input cost |
| **Effort and ceiling tuning** | Downshift reasoning/thinking budgets and cap `max_tokens` to observed p95 | Large on reasoning models |
| **Response cache** | Return the stored answer for an identical request | 100% on hits |

The second one is worth calling out because it is the least visible and often the largest: providers charge a fraction of the normal input price to read a cached prompt prefix, but only if breakpoints are placed correctly and the conversation keeps hitting the same endpoint. Relay does both. A naive cost-optimizing router that ignores cache affinity will migrate a conversation to a "cheaper" model and increase the bill.

---

## How adoption works

```diff
- base_url = "https://api.openai.com/v1"
+ base_url = "https://relay.example.com/v1"
```

That is the whole integration. Keep sending `model: "gpt-4o"`.

Relay treats the model you asked for as a **baseline and a ceiling**: it will not serve you something above it, and it will serve you something below it only when the request clears your quality floor. Every response tells you what actually happened:

```http
X-Relay-Served:     openai/gpt-4o-mini@us-east
X-Relay-Baseline:   openai/gpt-4o@us-east
X-Relay-Cost-Usd:   0.000210
X-Relay-Baseline-Usd: 0.003480
X-Relay-Saved-Usd:  0.003270
```

Three ways to stay in control:

- **`X-Relay-Pin: strict`** on any request — served exactly as asked, no optimization. Always honored.
- **Shadow mode** — Relay serves exactly what you asked for and only *reports* what it would have saved. Run it for a week before enabling anything.
- **Quality floor** — a hard constraint, not a preference. Endpoints below it are eliminated, never merely ranked lower.

See [ADR-0007](docs/adr/0007-requested-model-as-baseline.md) for why substitution is opt-in and always disclosed.

---

## Architecture at a glance

Relay separates a stateless **data plane** on the hot path from a **control plane** holding configuration and accounting.

```
                         ┌──────────────────────────────────┐
   OpenAI-compatible     │           DATA PLANE             │
   clients ─────────────▶│                                  │
   (any SDK, any lang)   │  auth ▸ limits ▸ validate ▸       │
                         │  normalize                       │
                         │            │                     │
                         │            ▼                     │
                         │  ┌──────────────────┐            │
                         │  │ Optimizer        │            │
                         │  │ cache breakpoints│            │
                         │  │ effort / ceilings│            │
                         │  └────────┬─────────┘            │
                         │           ▼                      │
                         │  ┌──────────────────┐            │
                         │  │ Router  (pure)   │◀── catalog │
                         │  │ filter▸score▸rank│◀── policy  │
                         │  └────────┬─────────┘◀── health  │
                         │           │ Decision + Baseline  │
                         │           ▼                      │
                         │  ┌──────────────────┐            │
                         │  │ Executor         │            │
                         │  │ retry / failover │            │
                         │  │ cascade escalate │            │
                         │  └────────┬─────────┘            │
                         │           ▼                      │
                         │  ┌──────────────────┐            │
                         │  │ Provider adapters│            │
                         │  │ OpenAI Anthropic │            │
                         │  │ Gemini  Ollama   │            │
                         │  └────────┬─────────┘            │
                         │           │                      │
                         │   normalize ▸ stream ▸ meter     │
                         └───────────┬──────────────────────┘
                                     │ usage + savings + decisions
                         ┌───────────▼──────────────────────┐
                         │          CONTROL PLANE           │
                         │  tenants · API keys · budgets    │
                         │  model catalog · routing policy  │
                         │  savings ledger · audit          │
                         │  admin API · analytics           │
                         └──────────────────────────────────┘
```

Three properties matter more than the boxes:

**Routing is a pure function.** `Router.Route(request, catalog, policy, health) → Decision` performs no I/O. It is a deterministic transformation of inputs into a ranked list with reasons attached, which makes it exhaustively testable and makes the explainability API free — the `Decision` *is* the explanation.

**Execution is separate from routing.** The `Executor` takes a `Decision` and carries it out: retries, failover, cascade escalation, streaming. Routing never knows about HTTP; execution never re-derives preferences.

**Every request records both costs.** What it cost, and what the baseline would have cost. That difference is the product.

Full detail in [`docs/architecture.md`](docs/architecture.md).

---

## Deployment

**Hosted SaaS** is the default: point at Relay's endpoint, bring your own provider keys. Relay never fronts your provider spend and never retains prompt content.

**Self-hosted** for regulated or high-sensitivity environments: run the same binary in your own VPC, prompts never leave your network. Same control plane, same admin API.

Relay is a hard dependency in your critical path, so it is built to **fail open**: if Relay is degraded, requests pass through to the baseline model unrouted rather than failing. Being unable to optimize must never mean being unable to serve.

---

## Documentation

| Document | Contents |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | Components, core types, optimizer, streaming, reliability, state, SLOs |
| [`docs/routing.md`](docs/routing.md) | Baseline semantics, quality floor, scoring, catalog, savings accounting |
| [`docs/roadmap.md`](docs/roadmap.md) | Build order, and what is explicitly out of scope |
| [`docs/adr/`](docs/adr/) | Architecture decision records, including open decisions |

---

## Stack

Go · Chi · Viper · Zap · Postgres · Redis · Prometheus · OpenTelemetry · Docker

---

## Design principles

- **Never substitute silently.** Every swap is disclosed, every saving is attributable, `strict` is always honored.
- **Quality is a floor, not a preference.** Cost optimization that degrades output is not a saving.
- **Prove the saving.** A cost-reduction product that cannot measure the counterfactual is asking for trust it hasn't earned.
- **Provider logic lives only in adapters.** Nothing above the adapter layer branches on vendor name.
- **Decisions are data.** Routing produces a struct, not a side effect.
- **Fail open.** Degraded optimization beats a failed request.

---

## License

Not yet chosen.
