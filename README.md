# Relay

> **Cut your LLM bill without changing your code.** Point your OpenAI SDK at Relay, keep calling the models you already call, and Relay serves each request from the cheapest option that still meets your quality bar — then shows you exactly what it saved.

Relay is an AI gateway that sits between your applications and your model providers. Applications talk to it with the OpenAI SDK they already have, sending the model names they already send. Relay normalizes the request, optimizes it, routes it to the cheapest endpoint that clears the quality floor, and streams the answer back unchanged in shape.

The two things that make it a product rather than a proxy: **it never substitutes silently** — every swap is disclosed in response headers with the saving attached — and **it measures the counterfactual**, so "we cut your spend 38%" is a number you can audit rather than a claim you have to believe.

---

## Status

**Phases 1–5 built; phases 6–8 not started.** Relay serves `/v1/chat/completions` (streaming and non-streaming), `/v1/models`, `/healthz`, and `/readyz` against Ollama, OpenAI, and Anthropic, and records a per-request savings ledger.

All three modes work, per tenant. `strict` serves exactly what was asked. `shadow` also serves exactly what was asked and records what a cheaper route *would* have cost on the same tokens, so a customer can measure a month of savings before granting permission to change anything. **`optimize` substitutes** — within the quality floor, never above the requested model, always disclosed in the response headers, with cascade escalation when a downgrade returns invalid output. The optimizer levers (cache breakpoint insertion, output ceilings, effort downshift) and the exact-match response cache work in `strict` mode too, because they change the request rather than the model.

Still open, and both need production traffic rather than more code: reconciling the reported cost against a real provider invoice to within 1%, and confirming the escalation rate stays under the 2% SLO. The [roadmap](docs/roadmap.md) records exactly what is verified and what is not, per phase.

```sh
docker compose up                                 # gateway :8080, console :3000, local model

go test ./...
go run ./cmd/relay -validate                      # check config and price attestations, then exit
go run ./cmd/relay                                # data plane :8080, admin 127.0.0.1:9090

curl -s localhost:9090/savings?tenant=acme        # the savings report
curl -s localhost:9090/metrics                    # Prometheus
```

**`saved` and `shadow_saved` are different numbers and are never summed.** In shadow mode the baseline is served, so nothing is actually saved; `shadow_saved` is what optimization *would* have saved. That separation runs from the ledger through the metrics to the report, because reporting the second as the first would tell a customer they had banked money they had not.

Prices in `config/catalog.yaml` were verified against each provider's published pricing page on 2026-08-23 and carry that date as an attestation; the loader refuses one older than 90 days, and [Prices expire on purpose](#prices-expire-on-purpose) explains why that is a feature and how to stay ahead of it. Re-verify before pointing this at real traffic anyway — a price can change the day after it is checked, and every saving this product reports is arithmetic over these numbers. Tenant API keys live in `config/tenants.yaml` as SHA-256 digests; a missing file means every request is anonymous, which is what Phase 1 did.

The [roadmap](docs/roadmap.md) describes what gets built in what order; [`docs/adr/`](docs/adr/) records the decisions, including the ones still open.

---

## Quickstart

Nothing below needs a provider API key, and none of it costs anything.

```sh
docker compose up
```

That brings up the gateway on `:8080`, the console on `:3000`, and a local Ollama with
`qwen2.5-coder` pulled for you. Open <http://localhost:3000> and press one of the presets.

**Give Docker at least 8 GiB of memory, and expect a 4.7 GB download on first run.** The model is
a 7-billion-parameter one and needs roughly 5-6 GiB resident. With less, the failure is worth
recognising because it does not look like a memory problem: the download succeeds, the gateway
starts, the routing and the savings arithmetic are all correct, and only the answer itself never
arrives —

```
{"error":{"message":"{\"error\":\"llama-server process has terminated: signal: killed\"}",
          "type":"api_error","code":"RetrySame"}}
```

That is the model process being killed for memory. Docker Desktop grants a fraction of host RAM by
default; raise it under **Settings → Resources**. On Linux the container sees host memory directly
and only a small host needs attention.

Everything except the last step works regardless: the dry-run below explains a decision in full on
a machine that cannot load the model at all, which is most of what there is to see.

### Seeing the decision without spending anything

The most useful thing Relay does is explain itself, and the explanation is free — no provider is
called, nothing is billed, and nothing is written to the ledger:

```sh
curl -s localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-relay-demo-optimize-0f1e2d3c4b5a' \
  -H 'X-Relay-Dry-Run: 1' \
  -H 'X-Relay-Assume-Credentials: all' \
  -d '{"model":"relay/fast-coder","messages":[{"role":"user","content":"write a bash one-liner"}]}' \
| jq '{chosen, baseline,
       ranked:   [.ranked[]   | {endpoint, total, estimated_cost_usd}],
       rejected: [.rejected[] | {endpoint, reason}],
       saved: .estimate.estimated_saved_usd}'
```

Two headers there are worth explaining, because the command does nothing interesting without
either of them.

**The `Authorization` header is a Relay *tenant* key, not a provider key.** It names the demo
tenant from `config/tenants.yaml`, which is the only one configured in `optimize` mode. Without
it the request is anonymous, anonymous means `strict`, and strict serves exactly what was asked
with no comparison to show. Pinning `X-Relay-Pin: optimize` is deliberately *not* a substitute:
permission to serve a model you did not ask for belongs to the tenant, and a per-request header
is not where that consent lives.

**`X-Relay-Assume-Credentials: all`** asks the router to rank as though every credential were
configured, so the arithmetic is visible on a machine that holds no keys. It is honored **only** on
a dry run and is ignored on every live request — a header that could send real traffic to an
endpoint Relay cannot authenticate to would be a self-inflicted outage. The response's
`credentials` block reports which refs you actually hold, so the ranking stays complete without
pretending you could act on all of it.

### Sending a real request

Still no key required — the local model is free and the demo route will pick it:

```sh
curl -sN localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-relay-demo-optimize-0f1e2d3c4b5a' \
  -d '{"model":"relay/zero-key-demo","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

To use a paid provider, either set `RELAY_CRED_OPENAI_PRIMARY` in the environment (see
`.env.example`) or send the key on the request itself, which is what the console's *Try live* tab
does and what a hosted deployment uses:

```sh
-H 'X-Relay-Credential: openai-primary sk-...'
```

That header is a **provider** key. `Authorization` is a *Relay tenant* key and a different thing;
the two are never interchangeable. A request-supplied key is used for that request and is never
stored, never logged, and never written to the savings ledger.

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
X-Relay-Endpoint:       openai/gpt-4o-mini@us-east
X-Relay-Baseline:       openai/gpt-4o@us-east
X-Relay-Mode:           optimize
X-Relay-Substituted:    true
X-Relay-Cost-Usd:       0.000210
X-Relay-Baseline-Usd:   0.003480
X-Relay-Saved-Usd:      0.003270
```

`X-Relay-Substituted` means a **different model** answered. A recovery onto another region or
credential running the model you asked for is `X-Relay-Failover` instead — reporting that as a
substitution would tell you that you had been downgraded when you had not. The full ranking,
including everything that was rejected and why, is on `X-Relay-Decision`, and
[`X-Relay-Dry-Run`](#seeing-the-decision-without-spending-anything) returns all of it without
calling a provider at all.

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

## Prices expire on purpose

Every priced endpoint in `config/catalog.yaml` carries a `verified_on` date, and the loader
**refuses to start** when the oldest one is beyond `-max-price-age` (default 90 days).

This is deliberate and it is not a warning. Every number this product reports — the saving, the
baseline, the ranking that produced the decision — is arithmetic over those prices. Serving traffic
on a price nobody has confirmed for three months does not produce a slightly stale savings figure;
it produces a confident one that is wrong, and a routing decision made on the wrong grounds. The
failure has no other symptom, so the check is the symptom.

The consequence to plan for: **a deployment left alone will eventually refuse to boot.** That is the
intended behaviour and it should never be a surprise, so:

```sh
go run ./cmd/relay -validate     # prints the oldest attestation and days remaining, then exits
```

CI runs that weekly against a 76-day threshold, so the build goes red a fortnight before a running
gateway would. Re-verify the prices against each provider's published pricing page, update
`verified_on`, and the clock resets.

`-max-price-age` widens the window and `-max-price-age 0` disables the check entirely. Both are real
options for a private deployment whose operator has decided the trade, and neither is a default
here: turning it off means routing on numbers nobody has confirmed, and the savings report will not
say so.

---

## Documentation

| Document | Contents |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | Components, core types, optimizer, streaming, reliability, state, SLOs |
| [`docs/routing.md`](docs/routing.md) | Baseline semantics, quality floor, scoring, catalog, savings accounting |
| [`docs/roadmap.md`](docs/roadmap.md) | Build order, and what is explicitly out of scope |
| [`docs/adr/`](docs/adr/) | Architecture decision records, including open decisions |
| [`web/`](web/) | The console — an optional Next.js app; the gateway builds and runs without it |

---

## Stack

**What is actually here:** Go (standard-library `net/http` and `log/slog`) · `prometheus/client_golang` · `gopkg.in/yaml.v3` · Docker · Next.js, for the optional console.

That is the whole dependency list — three direct modules. Routing is `http.ServeMux`, configuration is command-line flags, logging is `slog`, and the savings ledger is append-only JSONL. The result is one static CGO-free binary, which is what makes the self-hosted deployment above a five-minute promise rather than a project.

**What is designed for and not yet built:** Postgres as the ledger's system of record and Redis for the shared response cache and rate limits, both named by [ADR-0005](docs/adr/0005-state-store-and-multi-instance.md) and scheduled for Phase 7; OpenTelemetry spans in Phase 6. Each arrives behind an interface that already exists, so today every one of them has a working in-process implementation instead. The consequence is stated rather than hidden: behind N replicas, the response-cache hit rate is roughly 1/N of what it could be, and `/savings` reports one instance's view.

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

[Apache-2.0](LICENSE). The reasoning, including why not MIT or AGPL, is in [ADR-0011](docs/adr/0011-license.md).
