# Routing

How Relay decides which model answers a request. This is the part of the system that justifies its existence, so it is specified in more detail than anything else.

---

## 1. The routing contract

Standard OpenAI SDKs give a gateway exactly one hook to route on: the `model` string. Everything else in the request body is defined by the OpenAI schema and cannot carry routing intent without breaking client libraries. So `model` carries it.

A `model` value is one of three things:

| Form | Example | Behavior |
|---|---|---|
| **Real model** | `gpt-4o-mini`, `claude-sonnet-5` | Resolved through catalog aliases to a specific endpoint. Pinned — no scoring, no classification. |
| **Virtual model** | `relay/fast-coder` | Names a **route**: a candidate list plus scoring weights. Scored. |
| **Auto** | `relay/auto` | A route that additionally requests classification to select weights and constraints. |

Precedence, in order:

1. **An explicit real model wins.** If the caller pinned a model, Relay uses it.
2. **Otherwise the named virtual model's route applies.**
3. **Otherwise the tenant's default route applies.**
4. **Tenant policy filters the candidate set at every level.**

The one rule that governs the interaction between (1) and (4): **policy may reject, but never substitute.** If a tenant is forbidden from using the model they pinned, the request fails with `403` and a message naming the policy — it does not quietly answer from a different model. A caller who asked for one model and silently received another has been given wrong information about their own system, and every downstream conclusion they draw from that response is unsound.

Relay-specific options that have no place in the OpenAI schema (session key, dry-run, weight overrides) travel as `X-Relay-*` headers or inside `extra_body`, so that an unmodified SDK never has to know they exist.

---

## 2. The model catalog

The catalog is **data**, not code. Adding a model is a config change reviewed like any other, not a deploy.

```yaml
# catalog.yaml
version: "2026-08-04.1"

endpoints:
  - id: anthropic/claude-sonnet-5@us-east
    provider: anthropic
    model: claude-sonnet-5
    deployment: us-east
    credential_ref: anthropic-primary

    capabilities:
      streaming: true
      tools: true
      json_schema: true
      vision: true
      reasoning: true
    limits:
      context_window: 200000
      max_output_tokens: 64000
      max_images_per_request: 20
    pricing:                      # USD per 1M tokens — illustrative, not authoritative
      input: 3.00
      cached_input: 0.30
      output: 15.00
    quality:                      # operator-supplied, 0..1, per dimension
      coding: 0.88
      reasoning: 0.90
      long_context: 0.85
      vision: 0.80
    lifecycle:
      status: ga                  # ga | preview | deprecated | retired
      deprecated_after: null
      replacement: null

aliases:
  claude-sonnet-5: anthropic/claude-sonnet-5@us-east
```

Three notes on the catalog:

- **Pricing goes stale and stale pricing routes wrongly.** Prices are versioned with the catalog, carry a `source` and `verified_on` field in the real schema, and a CI check fails the build when any entry has not been re-verified within its freshness window. Wrong prices are worse than no prices, because they produce confident wrong decisions.
- **Quality scores are operator judgment, and the docs say so.** They are not measured, they are asserted. They are the one subjective input in the scorer and they belong in config where they can be argued about, tuned per tenant, and eventually replaced by measured outcomes from the analytics store.
- **Lifecycle drives filtering.** A `deprecated` endpoint is scored with a penalty; a `retired` one is filtered out with a `Deprecated` reject reason naming its replacement.

---

## 3. Routes

```yaml
routes:
  - name: relay/fast-coder
    candidates:
      - anthropic/claude-sonnet-5@us-east
      - openai/gpt-4o-mini@us-east
      - google/gemini-flash@us-central
    require:
      - capability: tools
    weights:
      quality.coding: 0.40
      cost:           0.30
      latency:        0.20
      cache_affinity: 0.10
    fallback: anthropic/claude-sonnet-5@us-east
    max_attempts: 3
```

Weights must sum to `1.0`; config validation rejects anything else, because a set that sums to 1.3 produces scores that look comparable and are not.

`fallback` is what runs when the filter phase eliminates everything — the answer to "we couldn't satisfy the constraints" should be a defined, boring endpoint rather than an error, unless the route sets `fallback: none`.

---

## 4. Filter, then score

Two phases, kept strictly separate. **Hard constraints eliminate. Preferences rank.** Collapsing them — giving an unusable endpoint a low score instead of excluding it — means a bad enough shortage of alternatives will eventually select something that cannot serve the request at all.

### Phase 1 — filter

Each candidate is eliminated by the first constraint it fails, and the constraint is recorded:

| Constraint | Reject reason |
|---|---|
| `estimated_input + max_output > context_window` | `ContextTooSmall` |
| Request needs vision / tools / json_schema the endpoint lacks | `MissingCapability` |
| Tenant policy deny-list, or data-residency mismatch | `PolicyDenied` |
| No credential resolvable for this endpoint and tenant | `NoCredential` |
| Circuit breaker open for `(endpoint, credential)` | `CircuitOpen` |
| Estimated cost > `Policy.MaxCostPerRequest`, or tenant budget exhausted | `BudgetExceeded` |
| `lifecycle.status == retired` | `Deprecated` |

Rejections are returned in the `Decision`, not discarded. "Why didn't it pick X" is the most common routing question in production and this is the only cheap way to answer it.

### Phase 2 — score

Survivors are scored on normalized dimensions and combined with the route's weights:

```
score(e) = Σ  weight[d] × normalized[d](e)
```

Normalization rules:

- **Quality** dimensions are already `0..1` in the catalog; used as-is.
- **Cost and latency** are lower-is-better, normalized by inverted min–max **across the surviving set**:
  `norm(x) = (max − x) / (max − min)`
- **Degenerate range.** When `max == min` (including the single-survivor case), every candidate scores `1.0` for that dimension. Dividing by zero range is the obvious bug here.
- **Cache affinity** is `1.0` when the endpoint matches the session's previous endpoint, else `0.0`.

One property of min–max worth stating outright: normalization is **relative to the candidate set**. Adding a very expensive candidate to a route compresses everyone else's cost scores. Scores are therefore comparable *within* one decision and not *across* decisions — do not chart them over time as if they were a stable metric.

**Cost estimation** uses actual pricing against estimated tokens:

```
est_cost = (est_input_tokens / 1e6) × price.input
         + (est_output_tokens / 1e6) × price.output
```

with `est_output_tokens = min(max_tokens, route historical mean)`. These estimates route; they do not bill. See [architecture §8](architecture.md#8-token-and-cost-estimation).

**Latency** is an EWMA of observed request duration per endpoint, not a live probe. Routing performs no I/O.

**Ties** break deterministically: higher quality, then lower cost, then lexicographic endpoint ID. Never randomly — a router that returns different answers for identical inputs cannot be tested or debugged.

---

## 5. A worked example

Route `relay/fast-coder` as configured above. Request: a coding task, ~8,000 input tokens, `max_tokens: 1500`, tool calling required, part of a session whose previous turn was served by `claude-sonnet-5`.

**Candidates and catalog facts** (pricing per 1M tokens, illustrative):

| Endpoint | ctx | tools | in $ | out $ | quality.coding | latency EWMA |
|---|---|---|---|---|---|---|
| `anthropic/claude-opus-5@us-east` | 200k | yes | 15.00 | 75.00 | 0.95 | 4200 ms |
| `anthropic/claude-sonnet-5@us-east` | 200k | yes | 3.00 | 15.00 | 0.88 | 1800 ms |
| `openai/gpt-4o-mini@us-east` | 128k | yes | 0.15 | 0.60 | 0.62 | 900 ms |
| `google/gemini-flash@us-central` | 1M | yes | 0.10 | 0.40 | 0.60 | 700 ms |
| `ollama/qwen-coder@local` | 32k | **no** | 0.00 | 0.00 | 0.55 | 2500 ms |

**Phase 1 — filter.** Two eliminated:

- `ollama/qwen-coder@local` → `MissingCapability` (route requires `tools`)
- `google/gemini-flash@us-central` → `CircuitOpen` (breaker tripped 40s ago)

Three survive. All three fit 8,000 + 1,500 tokens comfortably.

**Estimated cost.** `(8000/1e6) × in + (1500/1e6) × out`:

| Endpoint | input cost | output cost | total |
|---|---|---|---|
| opus-5 | 0.1200 | 0.1125 | **$0.2325** |
| sonnet-5 | 0.0240 | 0.0225 | **$0.0465** |
| gpt-4o-mini | 0.0012 | 0.0009 | **$0.0021** |

**Normalization.** Cost: max `0.2325`, min `0.0021`, range `0.2304`. Latency: max `4200`, min `900`, range `3300`.

| Endpoint | quality | cost norm | latency norm | affinity |
|---|---|---|---|---|
| opus-5 | 0.95 | (0.2325−0.2325)/0.2304 = **0.000** | (4200−4200)/3300 = **0.000** | 0.0 |
| sonnet-5 | 0.88 | (0.2325−0.0465)/0.2304 = **0.807** | (4200−1800)/3300 = **0.727** | **1.0** |
| gpt-4o-mini | 0.62 | (0.2325−0.0021)/0.2304 = **1.000** | (4200−900)/3300 = **1.000** | 0.0 |

**Weighted totals** (0.40 quality, 0.30 cost, 0.20 latency, 0.10 affinity):

| Endpoint | quality | cost | latency | affinity | **total** |
|---|---|---|---|---|---|
| **sonnet-5** | 0.352 | 0.242 | 0.145 | 0.100 | **0.840** |
| gpt-4o-mini | 0.248 | 0.300 | 0.200 | 0.000 | **0.748** |
| opus-5 | 0.380 | 0.000 | 0.000 | 0.000 | **0.380** |

**Chosen: `anthropic/claude-sonnet-5@us-east`.**

Two things this example is here to show.

The best model loses. Opus has the highest coding quality on the list and finishes last, because at 5× the cost and 2.3× the latency of Sonnet it is not worth 0.07 of quality under these weights. That is the router working correctly, and it is also exactly the kind of outcome that will get reported as a bug — which is why the score breakdown is part of the response.

**Cache affinity is the margin.** Strip it out and Sonnet scores `0.740` against gpt-4o-mini's `0.748` — the cheap model wins by 0.008 and the conversation migrates mid-session. That migration discards the provider-side prompt cache built up over previous turns, and on a long conversation re-reading the whole prefix at full input price costs far more than the per-request difference the router just "saved". Cache affinity is not a tiebreaker bolted on for tidiness; it is a correction for a real cost the naive scorer cannot see.

---

## 6. Prompt-cache affinity

Providers cache prompt prefixes and charge substantially less to read from that cache than to process fresh input tokens. The saving grows with conversation length, which is precisely when routing is most tempted to switch.

Relay tracks the endpoint that served the previous turn of a session, keyed by `SessionKey` (client-supplied, or derived from a hash of the stable message prefix when absent). Affinity enters the scorer as a normal weighted dimension rather than a hard constraint, so a route can still migrate when the case is strong — an endpoint going unhealthy, a hard constraint failing, or a genuinely large score gap.

Affinity is capped: after a configured number of consecutive sticky turns the weight decays, so a session cannot be pinned forever to an endpoint that has since become a bad choice.

---

## 7. Classification

Classification applies to `relay/auto` routes only. It never runs for a pinned model or an explicitly named route, because in those cases the caller has already made the decision.

**It is an input to the scorer, not a stage of the pipeline.** It selects which quality dimension to weight and which capabilities to require — nothing more. It does not select an endpoint.

```
Classification
  TaskType     string      // code | reasoning | summarization | extraction | creative | ...
  Complexity   float64     // 0..1
  NeedsVision  bool
  NeedsTools   bool
  Confidence   float64     // 0..1
  Source       string      // rules | heuristic | model
```

Three rules keep it from becoming the latency and cost problem it naturally wants to be:

1. **Deadline-bounded.** Classification gets a hard budget (default 50 ms). On timeout, the route's default weights apply and the request proceeds. It may never be the reason a request is slow.
2. **Cached** by hash of the classification-relevant prefix. Multi-turn conversations classify once, not per turn.
3. **Low confidence falls back.** Below the confidence threshold, the route's default weights apply. A guess that is probably wrong is worse than no guess, because it routes with false precision.

The implementation progresses: rules → rules plus heuristics (length, code fences, language detection, tool presence) → a small local model. A model-based classifier that adds a full LLM round trip to every request is not a viable design for a component whose entire host has a 5 ms p50 overhead budget; if one is ever used it must be local, small, and still bound by rule 1. See [ADR-0006](adr/0006-classifier-placement.md).

---

## 8. Explainability

Every decision is inspectable, through three mechanisms.

**Response header.** Successful responses carry a compact summary:

```
X-Relay-Decision: route=relay/fast-coder; chosen=anthropic/claude-sonnet-5@us-east;
                  score=0.840; attempts=1; catalog=2026-08-04.1
X-Relay-Cost-Usd: 0.041920
```

**Dry run.** Any request may be sent with `X-Relay-Dry-Run: true`. Relay routes it fully and returns the complete `Decision` — ranked candidates with per-dimension breakdowns, rejected candidates with reasons — without calling a provider and without incurring cost. This is the tool for answering "what would happen if" and for testing policy changes before applying them.

```jsonc
{
  "route": "relay/fast-coder",
  "catalog_version": "2026-08-04.1",
  "policy_version": "tenant-42.7",
  "chosen": "anthropic/claude-sonnet-5@us-east",
  "ranked": [
    { "endpoint": "anthropic/claude-sonnet-5@us-east", "total": 0.840,
      "components": { "quality.coding": 0.352, "cost": 0.242,
                      "latency": 0.145, "cache_affinity": 0.100 },
      "reasons": ["session affinity from previous turn"] },
    { "endpoint": "openai/gpt-4o-mini@us-east", "total": 0.748, "components": { ... } },
    { "endpoint": "anthropic/claude-opus-5@us-east", "total": 0.380, "components": { ... } }
  ],
  "rejected": [
    { "endpoint": "google/gemini-flash@us-central", "reason": "CircuitOpen",
      "detail": "opened 40s ago, 7/10 failures" },
    { "endpoint": "ollama/qwen-coder@local", "reason": "MissingCapability",
      "detail": "route requires tools" }
  ]
}
```

**Decision history.** Decisions are persisted with their catalog and policy versions. Because `Route` is pure and the versions are recorded, any past decision can be replayed exactly — which turns "why did it pick that model last Tuesday" from an archaeology exercise into a query.

---

## 9. What routing deliberately does not do

- **No live probing during routing.** `Route` performs no I/O. Health and latency come from a snapshot maintained out of band.
- **No parallel execution, consensus, or voting.** Sending one request to N models multiplies cost by N for a benefit that is real only in narrow cases. See [roadmap](roadmap.md#explicitly-out-of-scope).
- **No hedging by default.** Same reason. If added, it is opt-in per route with the cost consequence documented where it is enabled.
- **No silent model substitution against an explicit pin.** Covered in §1, and worth repeating: it is the one behavior that would make Relay untrustworthy.
