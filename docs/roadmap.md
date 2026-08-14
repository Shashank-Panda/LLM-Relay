# Roadmap

Two ordering principles, and they point the same way.

**Build the pipes before the intelligence.** Classification is the most interesting component and the worst starting point — its requirements are the least knowable in advance and it is the most likely to be redesigned once real traffic exists. Nothing depends on it, so nothing is blocked by deferring it.

**Reach a sellable milestone as early as possible, and reach it without asking the customer to take any risk.** That means the order is: prove you can measure the saving → deliver savings that carry no quality trade → only then ask permission to substitute models.

The consequence is that **shadow mode ships before substitution does**, and **optimization ships before routing does**. A customer can adopt Relay, see a monthly savings figure, and take real savings — all before Relay has ever served them a model they did not ask for.

---

## Phase 1 — Vertical slice (passthrough gateway) — **built, pending live verification**

**Goal: one request goes end to end, streaming and not, against real providers. No optimization, no routing.**

- `POST /v1/chat/completions`, streaming and non-streaming
- `GET /v1/models`, `/healthz`, `/readyz`
- Three adapters: **Ollama** (free, local, fast to iterate against), **OpenAI**, and **Anthropic**
- Static YAML catalog at boot; **`strict` mode only** — serve exactly what was asked
- Baseline resolution wired in, even though everything is `strict` — the type exists from day one
- Normalization and denormalization including tool calls; SSE framing, flushing, `[DONE]`
- Client-disconnect cancellation propagating to the provider
- Structured logging with request IDs; graceful shutdown
- Recorded-fixture tests per adapter; `goleak` on all streaming tests

**Why first.** This is all the work that is tedious rather than interesting, and therefore underestimated: SSE edge cases, tool-call delta reassembly, cancellation, `bufio` buffer limits. Everything later depends on it, and none of it gets easier by waiting.

**Why three adapters rather than two.** OpenAI's wire format *is* Relay's public format, so that adapter alone would have validated nothing — a pass-through would pass every test. Anthropic shares almost no structure with it (top-level system prompt, tool results inside user messages, a seven-event streaming state machine, usage split across both ends of the stream), and `internal/provider/providertest` asserts that all three produce byte-identical normalized results from their own recorded fixtures.

**Done when:** an unmodified OpenAI SDK streams a tool-calling response through Relay from both providers, and cancelling the client visibly aborts the upstream call.

**Verification status.** Everything above is covered by fixtures and `httptest` mock providers — including incremental frame delivery and cancellation propagation — and by a manual smoke run of the binary. The **live** half of "done when" is still open, because it needs a running Ollama or a real provider key. To close it:

```sh
# Local, free — needs Ollama installed
ollama serve & ollama pull qwen2.5-coder
go run ./cmd/relay -catalog config/catalog.yaml
curl -N localhost:8080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"ollama/qwen-coder@local","messages":[{"role":"user","content":"hi"}],"stream":true}'

# Paid — one key, one provider
export RELAY_CRED_ANTHROPIC_PRIMARY=sk-ant-...   # or RELAY_CRED_OPENAI_PRIMARY
go run ./cmd/relay

# The SDK check that actually closes the milestone
pip install openai
OPENAI_BASE_URL=http://localhost:8080/v1 OPENAI_API_KEY=unused python - <<'PY'
from openai import OpenAI
tools=[{"type":"function","function":{"name":"get_weather",
  "parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]
for c in OpenAI().chat.completions.create(model="claude-sonnet-5", stream=True, tools=tools,
        messages=[{"role":"user","content":"weather in Paris?"}]):
    print(c.choices[0].delta)
PY
```

Prices in `config/catalog.yaml` are illustrative and must be re-verified before any of this is pointed at real traffic; the loader rejects an attestation older than 90 days.

---

## Phase 2 — Baseline and savings accounting (shadow mode) — **built, pending reconciliation**

**Goal: prove the saving before claiming it. First sellable milestone.**

- `Baseline` resolution across all three modes; `shadow` becomes usable
- Counterfactual pricing: every request records `Cost`, `BaselineCost`, `Saved`, `SavingMeasured`
- Per-request usage records reconciled from provider-reported actuals
- `X-Relay-Decision`, `X-Relay-Baseline`, `X-Relay-Saved-Usd` headers
- Core Prometheus metrics: cost, baseline cost, savings, gateway overhead separated from provider time
- A savings query good enough to produce a per-tenant monthly figure

**Why this early.** It is the product's central claim, and until it exists every savings number is an assertion. It is also the entire sales motion: a prospect runs Relay in shadow for a month, changes nothing, and gets a report saying what they would have saved. That is the lowest-risk possible way to adopt a vendor in your critical path.

**The invariant everything else is arranged around.** A shadow saving is not a saving. In shadow mode the baseline *is* served, so `saved` is genuinely zero and the counterfactual figure lives in a separate field — `shadow_saved` — from the ledger through the metrics to the report. Telling a customer that a month of shadow traffic had already banked money it had not is the single most damaging error this product could make, so the two numbers are never summable at any layer, and the report carries the caveat inline rather than only in documentation.

**What it needed that was not on the list.** Two things:

- *Tenancy.* A per-tenant report needs a tenant. `config/tenants.yaml` maps SHA-256-hashed API keys to tenants and their policies; an unkeyed request still works and is attributed to a default tenant, so Phase 1 deployments are unaffected. This is a thin slice of Phase 7 pulled forward, taken because a ledger written without the dimension would have needed re-keying — a migration on billing-adjacent data.
- *A control-plane listener.* `/metrics` and `/savings` are served on a separate address bound to loopback by default (`-admin-addr`), not on the data plane. Cost data is not public and there is no authentication on it until Phase 7, so the bind address is the control.

**Where the ledger lives.** [ADR-0005](adr/0005-state-store-and-multi-instance.md) names Postgres as the system of record but also requires the data plane to buffer and spill to disk when it is unreachable. Phase 2 builds that spill path first: an append-only JSONL ledger plus an in-memory aggregate. Phase 7 adds a Postgres sink behind the same interface and this one keeps running. Recording never blocks a request — records go to a buffered channel and are dropped with a counter when full, because a gateway that stalls inference to write telemetry has inverted its own priorities.

**Done when:** a week of shadow traffic produces a per-tenant savings report, and the reported cost reconciles against the provider's own dashboard within 1%.

**Verification status.** The report exists and is correct against fixtures and a live smoke run: a mixed shadow/strict/anonymous workload produces correct per-tenant, per-route, and per-day figures, with `saved` at zero and `shadow_saved` populated. Measured gateway overhead was ~28 µs mean against a 5 ms p50 target.

The **reconciliation** half is still open, and cannot be closed here — it requires a real provider invoice to compare against. To close it:

```sh
# Run shadow traffic for a billing period against a real provider, then:
jq -s 'map(select(.usage_estimated | not) | .cost_micros) | add / 1e6' data/ledger.jsonl
# Compare with the provider's own dashboard for the same window; the target is 1%.
# Exclude usage_estimated records first — their cost is derived, not reported.
```

Two known sources of drift to check when doing it: requests whose provider returned no usage block (counted as `estimated_usage_requests`, and excluded above), and dropped ledger records under load (`dropped_records` in the report, `relay_meter_dropped_total` in metrics — both should be zero).

---

## Phase 3 — Optimizer and caching

**Goal: real savings, with zero quality risk, still without substituting any models.**

- `Optimizer.Apply` with a 3 ms budget, failing open to passthrough
- **Cache breakpoint insertion** — the largest lever, and validated against the provider's reported `cached_input_tokens` rather than trusted
- Effort downshift and `max_tokens` ceilings, both bounded by policy and never overriding an explicit caller value
- Exact-match response cache, opt-in per route, streaming replayed as a stream
- `Decision.Optimizations` recorded and surfaced in headers and dry-run
- Context pruning behind a separate opt-in (it changes semantics)

**Cache correctness constraints** — the easiest place in the whole system to introduce a serious bug:
- **Never cache across tenants.** Prompts contain private data; a cross-tenant hit is a breach with a nice performance graph.
- **Never cache when `temperature > 0`** unless the route opts in — non-determinism is usually the point.
- **Key on everything affecting output:** model, system prompt, tools, `response_format`, `temperature`, `top_p`, `max_tokens`, `seed`.

**Why before routing.** These savings work in `strict` mode, carry no quality-floor risk, and require no trust from the customer. A cautious buyer takes them first.

**Done when:** provider-reported cached input tokens rise measurably after enabling breakpoint insertion, and a tenant in `strict` mode shows a positive measured saving.

---

## Phase 4 — Reliability and fail-open

**Goal: safe to be a hard dependency. Prerequisite for ever substituting a model.**

- Error taxonomy and per-adapter `ClassifyError`
- Retry with exponential backoff and jitter; honor `Retry-After`
- Failover across candidates, with the **pre-first-byte boundary** enforced and tested
- `Reroute` handling for `context_length_exceeded`
- Circuit breaker per `(endpoint, credential)` with half-open probes
- Per-attempt timeouts within a total request deadline; admission control and load shedding
- **Fail-open passthrough on every internal failure**, with `relay_degraded_total` and its alert ([ADR-0010](adr/0010-fail-open-availability.md))

**Done when:** fault injection produces correct behavior for every error class, and killing Postgres, Redis, and the Optimizer in turn degrades savings without dropping a single request.

---

## Phase 5 — Routing, quality floor, and substitution

**Goal: turn on the headline feature, with its guardrails already in place.**

- Catalog as versioned data; virtual models and routes
- Hard-constraint filtering with recorded reject reasons, including `BelowQualityFloor` and `AboveBaseline`
- Normalized weighted scorer with deterministic tie-breaking; prompt-cache affinity
- `optimize` mode enabled per tenant; `X-Relay-Pin: strict` honored
- **Cascade escalation** on validity failure, with negative savings recorded ([ADR-0009](adr/0009-quality-floor-and-cascade.md))
- Offline eval harness producing quality scores per task type
- `X-Relay-Dry-Run`; exhaustive table tests over `Route` — highest test density in the project

**Done when:** the worked example in [routing §7](routing.md#7-a-worked-example) reproduces exactly; a tenant switched from `shadow` to `optimize` shows the predicted saving materialize; and escalation rate stays under 2%.

---

## Phase 6 — Observability and reporting

- OpenTelemetry spans: request → optimize → route → attempt → provider call
- Full metric set including substitutions, escalations, optimizations, degradations
- Decision records persisted with catalog and policy versions, replayable
- Customer-facing savings report: per tenant, per route, per endpoint, drillable to individual requests

**Done when:** any line of a monthly savings report can be drilled to the requests that produced it.

---

## Phase 7 — Tenancy and control plane

- Postgres schema and migrations; tenants, API keys (hashed), scopes
- Admin API for catalog, routes, policies, quality floors, lever configuration
- Budgets with **reservation semantics**; per-tenant and per-credential rate limits in Redis
- `CredentialResolver` with a concrete BYOK implementation ([ADR-0004](adr/0004-credential-ownership.md))
- Audit log; opt-in prompt capture with redaction and retention
- Self-hosted packaging and signed catalog snapshot distribution

**Done when:** two tenants with different quality floors and budgets share one deployment, and neither can exceed its budget under concurrent load — the reservation model is the specific thing under test.

---

## Phase 8 — Classification

- Rule-based, then heuristics (length, code fences, language detection, tool presence)
- Prefix-hash caching, 50 ms hard deadline, low-confidence fallback
- Feeds weight and quality-dimension selection only — never endpoint selection

**Done when:** enabling it changes routing outcomes measurably, and disabling it changes p99 gateway overhead by an amount too small to see.

---

## Later, if justified

- Additional providers (Bedrock, Vertex, Azure OpenAI, Groq, Together)
- `/v1/embeddings`
- Online quality signals and per-tenant quality calibration replacing global asserted scores
- A small local model for classification
- Shared circuit-breaker state, if per-instance detection proves inadequate
- Kubernetes; Cloud Run / Fly.io before that

---

## Explicitly out of scope

Cut with reasons, so the reasons can be revisited rather than re-argued.

**Hedging.** Starting a second attempt before the first fails doubles spend on every request to improve a tail. For a cost-reduction product this is precisely backwards. Cascade escalation gets the recovery benefit sequentially, paying extra only when something actually failed.

**Parallel execution, consensus, majority voting, response merging.** Same arithmetic, worse: cost × N on every request. If a caller wants three opinions they can send three requests.

**Percentage-of-savings pricing.** Relay bills a per-request gateway fee. Tying revenue to measured savings would make the measurement an invoice input, requiring dispute handling and audit guarantees far beyond what a dashboard needs — and would create an incentive to report savings generously, which is the last incentive this product should have. The ledger is still built to be auditable, because pricing models change more easily than measurement infrastructure.

**Semantic cache.** Approximate prompt matching returns answers to questions that were not asked. The failure is silent and the debugging story is terrible. Exact-match only.

**LLM-as-judge in the request path.** Adds an inference to every request — the exact cost problem the product exists to solve. Viable offline on sampled traffic for eval purposes only.

**Self-improving classifier.** Needs a labelled outcome signal that does not exist yet. Revisit when the analytics store holds real feedback data.

**PII detection.** Low precision, high effort, moving target. Secret detection (regex over known key formats) is cheap and lands in Phase 7; general PII should be bought or delegated, later.

**OAuth and full RBAC.** Tenant-scoped API keys cover the requirement. Add roles when someone can name the role they need.

**Images and audio endpoints.** Different request shapes, pricing models, and failure modes. Not until chat is genuinely solid.

**Client SDKs, CLI, VS Code extension, Slack bot.** OpenAI compatibility means these already exist and work. Building them would be building competitors to software the customer already has — and would contradict the one-line-integration promise.

**Frontend dashboard.** Grafana over Prometheus and Postgres covers internal needs until the admin API is stable. The customer-facing savings report in Phase 6 is the exception, because it is the product's proof and cannot be outsourced to a Grafana link.

**RAG, vector databases, agent frameworks, workflow orchestration, fine-tuning.** Different products. A gateway that also does these does none of them well.
