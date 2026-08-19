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

## Phase 3 — Optimizer and caching — **built, pending live provider validation**

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

**What "strict mode" means here, because two documents said different things.** [Routing §1](routing.md#1-the-routing-contract) says a strict request gets "no optimization"; [ADR-0008](adr/0008-request-optimization.md) says the levers work in `strict` mode and that shipping them first is the point. Both are kept, by separating the two things that were being called strict:

- **Tenant `optimization_mode: strict`** means *no substitution*. Relay never serves a model the tenant did not name. It does not forbid rewriting the request sent to the model they did name — and a tenant gets levers only by configuring `policy.levers`, whose zero value enables none, so nothing changes for an existing deployment.
- **`X-Relay-Pin: strict` on a request** is the stronger statement: the one request where nothing may be touched. It disables the optimizer *and* the response cache. An escape hatch with exceptions is not one.

**What it needed that was not on the list.** The `max_tokens` ceiling is specified as "never below what the caller's own responses have historically needed", and nothing was measuring that — so the lever was correct code that declined to act on every request it ever saw. `meter.RouteStats` is a per-route histogram of observed output lengths, fed from the same `Record` as the ledger and the metrics and exposed to the optimizer as a `StatsSource`. It requires 50 samples before reporting a p95; it rounds *up* to a bucket edge, because a ceiling that lands under the observations it came from truncates the responses it was meant to accommodate; it decays old traffic, so a route whose prompts get rewritten re-learns its shape; and it returns "no basis" rather than a number for a route whose answers exceed its largest bucket, which is precisely the route that must not be capped. `/savings` reports it per route, because an absent entry there is the entire explanation for a lever that appears to do nothing.

**Where the response cache lives.** [Architecture §7](architecture.md#7-state) puts it in Redis, shared across replicas. Phase 3 builds the in-process path first, behind the same interface — the same sequencing as the Phase 2 ledger and for the same reason: the local implementation is what a single-instance deployment needs, and the shared one changes no call site when it arrives. The consequence is stated rather than hidden: behind N replicas the hit rate is roughly 1/N of what it could be.

**Done when:** provider-reported cached input tokens rise measurably after enabling breakpoint insertion, and a tenant in `strict` mode shows a positive measured saving.

**Verification status.** The second half is closed. A `strict`-mode tenant with `levers.preset: recommended` serves one request live and the identical repeat from cache, and `/savings` reports `saved_micros` equal to the full baseline cost with `shadow_saved_micros` at zero — a measured saving, not a counterfactual, for a customer who has granted no permission to substitute anything. Each cache correctness constraint has a test that fails when the rule is removed: cross-tenant isolation (twice, in the key and re-checked against the stored entry), the temperature rule, per-route opt-in, `X-Relay-No-Cache` in both directions, and the length-prefixed key encoding that keeps `("ab","c")` and `("a","bc")` apart. A cancelled stream stores nothing, because a partial answer in a cache looks complete to everyone who reads it afterwards.

The **first** half needs a real provider, because it is a claim about a vendor's behaviour rather than about this code. Breakpoint placement is provider-specific in its *effects*: the markers are encoded correctly for Anthropic's `cache_control` and OpenAI caches automatically, but whether a given placement actually produces cache reads is only knowable by asking. To close it:

```sh
# Enable breakpoints for a tenant, send the same long prefix twice against a
# real Anthropic key, then:
curl -s localhost:9090/savings | jq '{
  effort: .overall.breakpoints_inserted,
  effect: .overall.cached_input_tokens,
  share:  .cached_input_share
}'
# A run with effort above zero and effect at zero means the markers are in the
# wrong place and the lever is doing nothing but paying for cache writes. The
# report says so inline rather than leaving it to be inferred, because that
# failure is otherwise completely silent — the request succeeds and the bill is
# simply unchanged.
```

Two things to watch while doing it. `relay_output_truncated_total{ceiling="relay"}` should stay near zero: the output ceiling is the one lever here that can shorten a legitimately long answer, and a ceiling that fires regularly is wrong rather than working. And `relay_degraded_total{component="optimizer"}` should be zero — it counts the fail-open paths, and passthrough is silent by design, so without it the gateway can stop optimizing entirely and go on looking perfectly healthy.

---

## Phase 4 — Reliability and fail-open — **built, pending live fault drill**

**Goal: safe to be a hard dependency. Prerequisite for ever substituting a model.**

- Error taxonomy and per-adapter `ClassifyError`
- Retry with exponential backoff and jitter; honor `Retry-After`
- Failover across candidates, with the **pre-first-byte boundary** enforced and tested
- `Reroute` handling for `context_length_exceeded`
- Circuit breaker per `(endpoint, credential)` with half-open probes
- Per-attempt timeouts within a total request deadline; admission control and load shedding
- **Fail-open passthrough on every internal failure**, with `relay_degraded_total` and its alert ([ADR-0010](adr/0010-fail-open-availability.md))


**What "failover" is allowed to mean, because it collides with the strict-mode promise.** Failing over changes which endpoint answers, and a strict tenant's whole purchase is that they get the model they named. Resolving that by refusing failover in strict mode would make the safest setting the least available one; resolving it by allowing failover to any candidate would perform the substitution they declined during exactly the incident where they are least able to notice.

The resolution is that an endpoint is a `(model, deployment, credential)` triple, so those are two different moves:

- **Strict and shadow** fail over only to endpoints running the **same model** — another region, another credential. The model that answers is the one the caller named, so the promise holds exactly, and `X-Relay-Failover` discloses it as a recovery.
- **Optimize** already has permission to choose among the ranked candidates, so the ranking is the failover order.

The permitted set is computed by the router and recorded on the `Decision`, not derived by the executor. A decision's failover options are part of why it was made, and the executor should be reading a plan rather than inventing one mid-incident. `X-Relay-Substituted` now means *a different model answered* and nothing weaker — reporting a region failover as a substitution would tell a caller they were downgraded when they were not, and it would put regional recoveries into the substitutions figure on a savings report.

**Two bounds that could not be expressed with `context.WithTimeout`.** A streaming request has two lifetimes stacked on one context: opening the stream must be bounded, and generating the answer must not be. There is no way to stand a `WithTimeout` timer down without cancelling the context it guards, so both the attempt timeout and the total deadline are a `WithCancel` plus a stoppable `AfterFunc`, disarmed at the moment the stream opens and handed to the stream to cancel on `Close`. The first version of this used `defer cancel()` and killed every stream the instant execution returned — caught by a Phase 1 test, which is the argument for having written that test.

**What it needed that was not on the list.** The `latency` scoring dimension had always been read by the scorer and written by nothing, so every endpoint scored identically on it and the weight was spent on nothing. `internal/health` produces both signals from the same observation point, because a completed attempt is the only event that reports on either: circuit state per `(endpoint, credential)`, and a latency EWMA per endpoint fed from time-to-first-token on streams and total duration otherwise.

**Done when:** fault injection produces correct behavior for every error class, and killing Postgres, Redis, and the Optimizer in turn degrades savings without dropping a single request.

**Verification status.** The error-class matrix is a table test: `RetrySame` retries the same endpoint under bounded exponential backoff with full jitter, `RetryOther` moves on immediately, `Reroute` re-filters with a corrected estimate, and `Terminal` and `Cancelled` stop outright — each asserted on the exact sequence of provider calls, because asserting on the returned error alone would pass for an implementation that quietly made three of them. A provider's `Retry-After` overrides the computed backoff and is still capped. ADR-0003's boundary is pinned from both sides: a pre-first-byte failure moves to the next candidate, and a break after the first flushed chunk does not, with the residual counted as `relay_stream_failures_after_ttft_total` — the measurement the ADR asks for before that decision gets revisited.

Live against a fault-injecting mock, a dead `us-east` deployment produced `X-Relay-Failover: true`, `X-Relay-Attempts: 4`, and a 200 served from `eu-west`; `/health` showed the breaker open on the failing endpoint with the healthy one still closed and carrying a latency EWMA. Two bugs surfaced doing this rather than in review: `Meter.Record` sent on a closed channel during shutdown, turning a graceful drain into a panic — the exact class of internal failure ADR-0010 forbids surfacing — and `relay_retries_total` was labelled with the endpoint that *answered*, which would point an operator at the healthy provider during an incident caused by the broken one. Both are fixed, the first by never closing the channel and the second by a per-attempt hook, since a ledger record cannot carry a fact that differs per attempt.

The **fault drill** in the second half of "done when" is still open, and most of it is not buildable yet: there is no Postgres and no Redis to kill until Phase 7. What exists of it is covered — the optimizer's fail-open has a test that crashes a `StatsSource` mid-request, a stale catalog degrades to baseline passthrough rather than routing on unconfirmed prices, and an unwritable ledger costs the record rather than the request. To close the rest, once those dependencies exist:

```sh
# With traffic running, kill each dependency in turn and watch two numbers.
# The failure mode being tested for is a *quiet* one: everything below should
# keep returning 200s while the savings rate falls.
watch -n1 'curl -s localhost:9090/metrics | grep -E "relay_degraded_total|relay_requests_total"'

# And the alert that makes this real, because passthrough is silent by design:
#   rate(relay_degraded_total[5m]) > 0
# without it Relay can be optimizing nothing, saving nothing, and still look
# perfectly healthy on every other dashboard.
```

One thing to watch that the drill will not show: `relay_output_truncated_total` and `relay_stream_failures_after_ttft_total` are both low-rate signals that only matter in aggregate, so they need a week of real traffic rather than an afternoon of injected faults.

---

## Phase 5 — Routing, quality floor, and substitution — **built, pending live escalation-rate data**

**Goal: turn on the headline feature, with its guardrails already in place.**

- Catalog as versioned data; virtual models and routes
- Hard-constraint filtering with recorded reject reasons, including `BelowQualityFloor` and `AboveBaseline`
- Normalized weighted scorer with deterministic tie-breaking; prompt-cache affinity
- `optimize` mode enabled per tenant; `X-Relay-Pin: strict` honored
- **Cascade escalation** on validity failure, with negative savings recorded ([ADR-0009](adr/0009-quality-floor-and-cascade.md))
- Offline eval harness producing quality scores per task type
- `X-Relay-Dry-Run`; exhaustive table tests over `Route` — highest test density in the project


**What the cascade can and cannot do, stated before it is relied on.** The validity checks detect *invalid* output, never *worse* output. A cheaper model returning a well-formed, schema-valid, subtly inferior answer passes every one of them. So escalation is a floor on correctness and `QualityFloor` is the separate, upstream floor on quality — and the ordering matters, because a reader who takes the cascade as a quality guarantee has been given a stronger promise than the code makes.

One rule governs every check: **when in doubt, valid.** The two failure directions are not symmetric. A missed violation costs nothing — the response is returned as it would have been anyway. A false alarm buys a second provider call for output that was fine, which is money, latency, and a negative saving. Every check therefore either proves a violation or declines to judge. The refusal heuristic is the clearest case: `finish_reason: content_filter` is the provider stating a refusal, and text-matching refusal phrasing was left out because it is locale-specific, defeated by paraphrase, and fires on the perfectly good answer to "what should I say when I have to decline a request".

JSON Schema validation is a **documented subset** — type, required, properties, items, enum, `additionalProperties: false` — and every construct it does not understand is skipped rather than guessed at. `anyOf` means the document need only satisfy one branch, and a checker that tested the wrong branch would report a violation that is not one. Full schema validation is a dependency decision that belongs in its own ADR, not smuggled in under a validity check.

**How the loop closes.** ADR-0009 calls escalation rate the control signal, and that only means something if it feeds back. `internal/health` tracks each endpoint's escalation rate as a decaying ratio and exposes it as `EndpointHealth.QualityPenalty`, which is subtracted from the asserted score before the floor is applied and before the endpoint is scored. The penalty *is* the rate, bounded — no curve and no coefficient, so an endpoint returning invalid output on a fifth of requests has an effective quality 0.2 below what the catalog claims, and that is a number anybody can recompute from the ledger.

Two properties keep it from becoming its own failure mode. It is **only ever a penalty**: a low escalation rate is evidence of validity rather than quality, and an endpoint returning well-formed rubbish would otherwise earn a bonus. And it is **bounded** at 0.3, because an unbounded penalty during a bad afternoon would drive an endpoint's effective quality to zero and remove it from every route carrying a floor — converting a quality signal into an outage.

**The eval harness produces the input the floor depends on.** `cmd/relay-eval` runs a suite of objectively-graded cases per task type against real endpoints and emits the catalog `quality:` block, with an attestation. Two constraints on what an eval may assert: no model grades another model, even offline, because a score produced by a judge is a claim about the judge and cannot be reproduced by a customer checking the number; and errored runs are excluded from the denominator rather than counted as failures, because a provider outage during an eval is not evidence about the model. Pasting the result into the catalog is deliberately manual — a score that rewrote the catalog automatically would let one bad afternoon at a provider silently change how every request routes.

**Done when:** the worked example in [routing §7](routing.md#7-a-worked-example) reproduces exactly; a tenant switched from `shadow` to `optimize` shows the predicted saving materialize; and escalation rate stays under 2%.

**Verification status.** The first two are closed. `TestWorkedExample_MatchesDocumentation` reproduces routing §7 to the published decimal — the ranking, the three totals, the winner's four component contributions, the estimated costs, and the 80% saving — and a companion test reproduces the document's own counterfactual, that stripping cache affinity flips the decision to the cheaper model by 0.008.

The shadow-to-optimize claim is a single test running the same request through both modes and asserting the figures are *identical* rather than close: shadow predicts `X-Relay-Shadow-Saved-Usd: 1.080000` while saving nothing, optimize realises `X-Relay-Saved-Usd: 1.080000`. They are the same subtraction over the same catalog, so anything less than equality would mean every shadow report ever shown to a customer was a guess.

Live, an escalating downgrade produced `X-Relay-Escalated: empty_completion`, `X-Relay-Attempts: 2`, and `X-Relay-Saved-Usd: -0.120000`; `/savings` reported `cost 1320000, baseline 1200000, saved -120000, discarded 120000` — the customer paid for two answers and used one, and the ledger says so. `relay_escalations_total` labels the endpoint that *failed*, not the baseline that rescued the request, for the same reason the retry counter does.

The **2% SLO** is the part that cannot be closed here, because it is a claim about production traffic rather than about this code. Everything needed to measure it exists: the rate is reported per route on `/savings` and as `relay_escalations_total`, measured against substitutable requests rather than all of them — against total requests a tenant running mostly strict traffic would show a rate near zero however badly their downgrades were doing. To close it:

```sh
# Per route, over a window, against the requests that were actually eligible.
curl -s localhost:9090/savings | jq '{
  rate: .escalation_rate,
  escalations: .overall.escalations,
  eligible: .overall.substitutable_requests,
  wasted: .overall.discarded_cost_micros
}'

# The alert that matters, because a rising rate is a quality regression that
# shows up on an invoice before anyone complains:
#   rate(relay_escalations_total[1h]) / rate(relay_substitutions_total[1h]) > 0.02
```

Two things a week of traffic will settle that a test cannot. Whether the quality-penalty bound of 0.3 and its 50-request minimum are the right numbers — both are defensible and neither is measured. And whether the validity checks have a false-positive rate at all: every one of them is conservative by construction, but "conservative by construction" is an argument, and the escalation rate on a route serving a model that is genuinely fine is the measurement.

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
