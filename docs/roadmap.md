# Roadmap

The ordering principle: **build the pipes before the intelligence.**

Intelligent routing is the reason Relay exists, which creates a strong pull toward building the classifier first. That is the wrong order. Classification is the component most likely to change shape as the rest of the system teaches you what it actually needs, and it is the least useful thing to have working when nothing can yet stream a token from a provider to a client. Building it first guarantees rebuilding it.

So: the boring transport work first, the decision engine second, then reliability, then everything that makes it operable, then the classifier last.

---

## Phase 1 — Vertical slice

**Goal: one request goes end to end, both streaming and not, against two real providers.**

- `POST /v1/chat/completions`, streaming and non-streaming
- `GET /v1/models`, `/healthz`, `/readyz`
- Two adapters: **Ollama** (free, local, fast to iterate against) and one paid provider
- Static YAML catalog loaded at boot; **explicit model pin only** — no routing logic at all
- Request normalization and response denormalization, including tool calls
- SSE framing, flushing, heartbeats, `[DONE]`
- Client-disconnect cancellation propagating to the provider
- Structured logging with request IDs; graceful shutdown
- Recorded-fixture tests for both adapters; `goleak` on all streaming tests

**Why first.** This phase contains all the work that is tedious rather than interesting and therefore gets underestimated: SSE edge cases, tool-call delta reassembly, cancellation, `bufio` buffer limits. Every later phase depends on it and none of it becomes easier by waiting.

**Done when:** an unmodified OpenAI SDK talks to Relay, streams a tool-calling response from both providers, and cancelling the client aborts the upstream call — verified by watching the provider-side request terminate.

---

## Phase 2 — Routing core

**Goal: the thing that makes this a router and not a proxy.**

- Catalog as versioned data with capabilities, limits, pricing, quality, lifecycle
- Virtual models and routes; the routing contract precedence from [routing §1](routing.md#1-the-routing-contract)
- Hard-constraint filtering with recorded reject reasons
- Normalized weighted scorer, deterministic tie-breaking
- The `Decision` struct, `X-Relay-Decision` header, and `X-Relay-Dry-Run`
- Config hot-reload behind an atomic snapshot pointer
- Exhaustive table tests over `Route` — this is where test density should be highest in the whole project

**Done when:** dry-run returns a full ranked-and-rejected explanation for a request, and the worked example in [routing §5](routing.md#5-a-worked-example) reproduces exactly.

---

## Phase 3 — Reliability

**Goal: a provider having a bad day is not an outage.**

- Error taxonomy and per-adapter `ClassifyError`
- Retry with exponential backoff and jitter; honor `Retry-After`
- Failover across ranked candidates, with the **pre-first-byte boundary** enforced and tested
- `Reroute` handling for `context_length_exceeded`
- Circuit breaker per `(endpoint, credential)` with half-open probes
- Per-attempt timeouts inside a total request deadline
- Admission control: bounded in-flight, bounded queue, shed with `503`

**Done when:** fault injection produces the correct behavior for every error class, and a provider failing mid-stream terminates that stream with an error event rather than hanging or silently truncating.

---

## Phase 4 — Observability

**Goal: you can answer why a request was slow, expensive, or wrong.**

- Prometheus metrics, with gateway overhead measured **separately** from provider time
- OpenTelemetry spans: request → route → attempt → provider call
- One usage record per request, reconciled from provider-reported actuals
- Decision records persisted with catalog and policy versions
- Async metering pipeline that drops rather than blocks under pressure

**Done when:** a dashboard shows p50/p99 gateway overhead distinct from provider latency, and cost per tenant per endpoint reconciles against a provider invoice.

---

## Phase 5 — Tenancy and control plane

**Goal: safe to point more than one team at.**

- Postgres schema and migrations
- Tenants, API keys (hashed at rest), scopes
- Admin API for catalog, routes, policies
- Budgets with **reservation semantics** (reserve estimate → reconcile actual)
- Per-tenant and per-credential rate limits in Redis
- Audit log
- `CredentialResolver` with at least one concrete implementation — see [ADR-0004](adr/0004-credential-ownership.md), still open
- Opt-in prompt logging with redaction and retention

**Done when:** two tenants with different policies and budgets share one deployment, and neither can exceed its budget under concurrent load — the reservation model is the specific thing being tested here.

---

## Phase 6 — Caching

**Goal: stop paying twice for the same answer.**

- Prompt-cache affinity wired into the scorer and measured — the larger win, and the reason this phase is not just about response caching
- Exact-match response cache, **opt-in per route**, keyed on the full normalized request *including tenant, model, and every sampling parameter*
- Streaming responses replayed as streams from cache
- Cache metrics

**Cache correctness constraints**, since this is the easiest place to introduce a serious bug:
- Never cache across tenants. Prompts contain private data; a cross-tenant hit is a data breach with a performance graph.
- Never cache when `temperature > 0` unless the route opts in explicitly — non-determinism is usually the point.
- Key on everything that affects output: model, system prompt, tools, `response_format`, `temperature`, `top_p`, `max_tokens`, `seed`.

---

## Phase 7 — Classification

**Goal: `relay/auto` earns its name.**

- Rule-based classifier, then heuristics (length, code fences, language detection, tool presence)
- Prefix-hash caching, hard deadline, low-confidence fallback
- Feed classification into weight and constraint selection only

**Done when:** enabling and disabling classification changes routing outcomes measurably, and disabling it changes p99 gateway overhead by an amount too small to see.

---

## Later, if justified

- Additional providers (Bedrock, Vertex, Azure OpenAI, Groq, Together)
- `/v1/embeddings`
- A small local model for classification
- Measured quality scores replacing operator-asserted ones, from analytics
- Shared circuit-breaker state across instances, if per-instance detection proves inadequate
- Kubernetes deployment; Cloud Run / Fly.io before that

---

## Explicitly out of scope

Cut with reasons, so that the reasons can be revisited rather than re-argued.

**Multi-provider execution strategies** — parallel execution, fastest-response-wins, consensus routing, majority voting, best-answer selection, response merging. Every one multiplies cost by N per request. The benefit is real in narrow, high-stakes cases and nowhere near broad enough to justify the complexity in a general gateway. If a caller wants three opinions, they can send three requests.

**Self-improving classifier.** Requires a labelled outcome signal that does not exist yet. Revisit once the analytics store has real feedback data — not before, because without labels this is just a slower rule engine with worse explainability.

**Semantic cache.** Approximate matching on prompts returns answers to questions that were not asked. The failure mode is silent and the debugging story is terrible.

**PII detection.** Low precision, high effort, and a moving target. Secret detection (regex for known key formats) is cheap and lands in Phase 5; general PII should be bought or delegated to a library, later.

**OAuth and full RBAC.** API keys scoped to a tenant cover the actual requirement. Add roles when someone can name the role they need.

**Images and audio endpoints.** Different request shapes, different pricing models, different failure modes. Not until chat is genuinely solid.

**Hedging by default.** Doubles spend to shave a tail. Opt-in per route at most, never on by default.

**Client SDKs, CLI, VS Code extension, Slack bot.** OpenAI compatibility means these already exist and work. Building them would be building competitors to software the user already has.

**Frontend dashboard.** Grafana against Prometheus and Postgres covers this until the admin API is stable. Building a UI on top of a moving API wastes the UI twice.

**RAG, vector databases, agent frameworks, workflow orchestration, fine-tuning.** Different products. A gateway that also does these is a gateway that does none of them well.
