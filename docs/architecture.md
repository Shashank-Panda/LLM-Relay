# Architecture

This document describes how Relay is built. It assumes you have read the overview in the [README](../README.md).

---

## 1. Data plane and control plane

Relay is two systems that happen to ship in one binary.

**The data plane** serves `/v1/*` traffic. It is stateless with respect to durable storage: everything it needs to answer a request — the model catalog, routing policy, tenant limits, credentials — is held in memory as an immutable snapshot, refreshed out of band. It never blocks a request on a database write. Its performance budget is measured in single-digit milliseconds.

**The control plane** owns everything durable: tenants, API keys, provider credentials, budgets, the model catalog, routing policies, usage records, and audit logs. It exposes an admin API and it publishes snapshots to the data plane.

The two communicate in exactly two directions:

- Control plane → data plane: **configuration snapshots** (catalog, routes, policies, limits), pushed or polled, versioned.
- Data plane → control plane: **usage and decision records**, written asynchronously off the hot path.

Keeping this boundary sharp is what allows the data plane to be scaled horizontally and restarted freely, and it is why a slow Postgres cannot take down inference traffic.

---

## 2. Request lifecycle

The data plane is a middleware chain terminating in a router and an executor:

```
HTTP request
  │
  ├─ recover / request ID / access log
  ├─ authenticate            → Principal{tenant, key, scopes}
  ├─ rate limit + admission  → 429 / 503 early
  ├─ decode + validate       → 400 early, before any expensive work
  │                            (max body size enforced here, not after parsing)
  ├─ normalize               → NormalizedRequest (provider-neutral)
  ├─ policy pre-check        → budget available? model permitted?
  │
  ├─ Router.Route(...)       → Decision (pure, no I/O)
  │
  ├─ Executor.Execute(...)   → attempts, retries, failover, streaming
  │     └─ Adapter.Chat / Adapter.ChatStream → provider HTTP
  │
  ├─ denormalize             → OpenAI-shaped response or SSE stream
  └─ meter                   → usage + decision record queued (async)
```

Two things are deliberately *not* in this chain. There is no synchronous classification stage — classification is an input the router may request for `auto` routes, bounded and cached (see [routing](routing.md#classification) and [ADR-0006](adr/0006-classifier-placement.md)). And there is no synchronous write to durable storage; metering is queued.

---

## 3. Core types

These are the load-bearing types. They should translate to Go structs with no further design work.

### NormalizedRequest

The provider-neutral form of an inbound request. Everything downstream operates on this; nothing downstream reads the raw HTTP body.

```
NormalizedRequest
  RequestID     string
  Tenant        TenantID
  Principal     Principal          // API key, scopes
  ModelSpec     ModelSpec          // what the caller asked for (see routing contract)
  Messages      []Message          // roles, content parts (text | image | tool_result)
  System        []ContentPart
  Tools         []ToolDef
  ToolChoice    ToolChoice
  ResponseFormat ResponseFormat    // text | json_object | json_schema
  Params        SamplingParams     // temperature, top_p, max_tokens, stop, seed, ...
  Stream        bool
  SessionKey    string             // optional; drives prompt-cache affinity
  Estimate      Estimate           // approximate input tokens, approximate output tokens
  Metadata      map[string]string
```

`Estimate` is explicitly approximate — see [§8](#8-token-and-cost-estimation).

### ModelEndpoint

**The routing unit.** Not a provider — a specific, callable, priced thing.

```
ModelEndpoint
  ID            string             // stable: "anthropic/claude-sonnet-5@us-east"
  Provider      ProviderID
  Model         string             // the provider's own model identifier
  Deployment    string             // region / Azure deployment / self-hosted instance
  CredentialRef string             // resolved at execution time, never stored inline

  Capabilities  Capabilities       // modalities, tools, json schema, streaming, ...
  Limits        Limits             // context window, max output tokens, max images
  Pricing       Pricing            // per-1M input / output / cached-input / reasoning
  Quality       QualityScores      // per-dimension, 0..1, operator-supplied
  Lifecycle     Lifecycle          // status, deprecation date, replacement endpoint
```

Circuit breakers, health, latency statistics, and rate limits are all keyed at this granularity — and, where credentials are per-tenant, at `(endpoint, credential)` granularity. A rate limit belongs to a key, not to a company. See [ADR-0001](adr/0001-model-endpoint-as-routing-unit.md).

### Catalog, Route, Policy

```
Catalog                             // immutable snapshot, versioned
  Version    string
  Endpoints  map[string]ModelEndpoint
  Aliases    map[string]string      // "gpt-4o" → canonical endpoint ID

Route                               // what a virtual model names
  Name       string                 // "relay/fast-coder"
  Candidates []CandidateRef         // endpoint ID + optional per-candidate overrides
  Weights    ScoringWeights         // must sum to 1.0
  Require    []Constraint           // route-level hard constraints
  Fallback   string                 // endpoint ID used when scoring yields nothing
  MaxAttempts int

Policy                              // tenant/org scoped, layered over the route
  Allow, Deny []Matcher             // provider, endpoint, region patterns
  MaxCostPerRequest  Money
  DataResidency      []Region
  AllowPromptLogging bool
```

Policy may **reject** a candidate. It may not silently substitute one. If policy forbids what the caller explicitly pinned, the request fails with a clear error rather than quietly answering from a different model — a caller who pinned a model and got another one has been lied to.

### Decision

The router's entire output, and the explainability payload.

```
Decision
  RouteName    string
  PolicyVersion string
  CatalogVersion string
  Ranked       []ScoredCandidate    // ordered best-first
  Rejected     []RejectedCandidate  // endpoint + constraint that eliminated it
  Chosen       string               // endpoint ID
  Classification *Classification    // present only if classification ran
  Deterministic bool

ScoredCandidate
  EndpointID string
  Total      float64
  Components map[string]float64     // per-dimension normalized contribution
  Reasons    []string

RejectedCandidate
  EndpointID string
  Reason     RejectReason           // ContextTooSmall | MissingCapability | PolicyDenied |
                                    // NoCredential | CircuitOpen | BudgetExceeded | Deprecated
  Detail     string
```

Recording `Rejected` is as important as recording `Ranked`. Most routing questions in production are "why *didn't* it pick X", and without this the answer requires reproducing the request.

### Attempt and Usage

```
Attempt
  EndpointID   string
  StartedAt    time.Time
  TTFT         time.Duration       // streaming only
  Duration     time.Duration
  Outcome      Outcome             // Success | Retryable | Terminal | Cancelled
  ErrorClass   ErrorClass          // see §6
  HTTPStatus   int

Usage
  InputTokens        int
  CachedInputTokens  int
  OutputTokens       int
  ReasoningTokens    int
  Cost               Money          // computed from actual usage × endpoint pricing
  Estimated          bool           // true if the provider returned no usage block
```

`Usage` comes from the provider's reported numbers wherever available. Estimates are for admission control only; billing and budget enforcement use actuals.

---

## 4. Provider adapters

The adapter interface stays small on purpose. If implementing a new provider is a day's work, provider coverage grows; if it's a week's work, it doesn't.

```go
type Adapter interface {
    ID() ProviderID
    Chat(ctx context.Context, req *NormalizedRequest, ep *ModelEndpoint, cred Credential) (*Response, error)
    ChatStream(ctx context.Context, req *NormalizedRequest, ep *ModelEndpoint, cred Credential) (Stream, error)
    ClassifyError(resp *http.Response, err error) ErrorClass
}

type Stream interface {
    Recv() (*Chunk, error)   // io.EOF terminates
    Usage() *Usage           // valid after EOF
    Close() error
}
```

`ClassifyError` is part of the interface because only the adapter knows what a given provider's error bodies mean. Everything else — retry policy, failover, breaker state — is generic and lives above it.

### What normalization actually costs

"Response normalization" is one box on a diagram and the largest body of code in the project. The genuinely hard parts, in rough order of difficulty:

1. **Tool calling.** Three vendors, three schemas. OpenAI emits `tool_calls` with JSON-string arguments accumulated across deltas; Anthropic emits `tool_use` content blocks with incremental JSON; Gemini uses `functionCall` parts. Argument fragments arrive split at arbitrary byte boundaries and must be reassembled before they parse. Round-tripping tool *results* back into the next request is a second, separate mapping.
2. **Structured output.** `json_schema` support, strictness semantics, and schema dialect differ per vendor. Some enforce, some only encourage.
3. **Reasoning / thinking content.** Separate content type, separately priced, sometimes redacted, sometimes required to be echoed back in the next turn.
4. **Multimodal parts.** URL vs base64, supported MIME types, per-image size and count limits.
5. **System prompts.** A message role in one API, a top-level field in another.
6. **Finish reasons and usage fields.** Small, tedious, and a frequent source of silently wrong metrics.

Adapters are tested against recorded provider fixtures — real captured responses replayed through the normalizer — plus a shared contract suite every adapter must pass.

---

## 5. Streaming

Streaming is the part most likely to be subtly broken, so its rules are explicit.

**Transport.** SSE, OpenAI chunk shape, terminated by `data: [DONE]`. Usage is emitted in a final chunk before `[DONE]` when the provider supplies it. Heartbeat comments keep intermediaries from timing the connection out. `X-Accel-Buffering: no` and immediate flush per chunk, because a buffering proxy turns streaming into a slow non-streaming response.

**Failover has a hard boundary at the first byte.** Before the first chunk is flushed to the client, Relay may transparently abandon an attempt and try the next candidate. After the first chunk is flushed, it may not — the client has already committed to a response. A post-first-byte failure terminates the stream with an error event. This is a real limitation, chosen deliberately over buffering-until-first-token, which would add TTFT to every request to protect a rare case. See [ADR-0003](adr/0003-streaming-failover-semantics.md).

**Cancellation propagates upstream.** When the client disconnects, the request context is cancelled and the upstream provider call is aborted. Without this you keep generating — and paying for — tokens nobody will read. This is the single most expensive bug a gateway can have, and it is invisible in testing.

**Reading the wire.** `bufio.Scanner` defaults to a 64KB line limit. Large tool-call argument payloads and base64 image echoes exceed it, and the failure mode is a truncated stream with no error. Use `bufio.Reader` with an explicit large buffer, or `Scanner` with `Buffer()` raised deliberately.

**Every stream has an owner and an exit.** No goroutine per stream that outlives the request context. Streams are closed on every path, including panic recovery, and goroutine count is monitored — leaked stream readers are the classic way a Go gateway dies slowly.

---

## 6. Reliability

### Error taxonomy

Retry policy without error classification burns money. Every provider error is mapped to exactly one class:

| Class | Examples | Action |
|---|---|---|
| `RetrySame` | 429 with `Retry-After`, 502/503/504, connection reset, read timeout | Back off, retry the same endpoint (respect `Retry-After` verbatim) |
| `RetryOther` | provider outage, breaker opened mid-flight, persistent 5xx | Next candidate in the ranked list |
| `Reroute` | `context_length_exceeded`, model deprecated/removed, capability unsupported | Re-filter with the corrected constraint, then pick a new candidate — do not retry |
| `Terminal` | 400 malformed, 401/403, content filter, invalid tool schema | Fail immediately, surface to caller |
| `Cancelled` | client disconnect, deadline exceeded | Abort, no retry |

`Terminal` is the class that matters most. Retrying a malformed request across three providers produces three bills and one guaranteed failure.

`Reroute` is the interesting one: `context_length_exceeded` is not a transient error and not a permanent one — it is a statement that the constraint set used for routing was wrong. Feeding it back into the filter and re-ranking is strictly better than either retrying or failing.

### Budgets and deadlines

- Each attempt has its own timeout; the request has a total deadline. Retries consume the total deadline and stop when it would be exceeded.
- Non-streaming retries carry an idempotency key where the provider supports one.
- `MaxAttempts` is per route and bounded. Every attempt is a real charge.
- Hedging (starting a second attempt before the first fails) is **not** implemented. It doubles spend for a latency improvement, and if it is ever added it must be opt-in per route with the cost consequence documented at the config site.

### Circuit breaker

Per `(endpoint, credential)`. Closed → open on an error-rate threshold over a rolling window, counting only `RetrySame`/`RetryOther` classes — a tenant sending malformed requests must not trip the breaker for everyone. Half-open admits a small probe quota before closing. Open endpoints are filtered out during routing rather than failed during execution, so the ranked list stays honest.

### Load shedding

Bounded in-flight concurrency per endpoint and globally, with a bounded queue and a queue deadline. When the queue is full, shed with `503` and `Retry-After` rather than accepting work that will time out anyway. Streaming connections are counted separately from non-streaming ones: they are long-lived and cheap in CPU but expensive in file descriptors and memory.

### Graceful shutdown

Stop accepting new requests, drain in-flight non-streaming requests, allow in-flight streams a bounded grace period, flush the metering queue, then exit. `/readyz` reports not-ready as soon as shutdown begins so load balancers stop sending traffic before the socket closes.

---

## 7. State

| State | Lives in | Why |
|---|---|---|
| Tenants, API keys, budgets, audit log, usage records, decision history | **Postgres** | Durable, queryable, transactional. This is the system of record. |
| Model catalog, routes, policies | **Postgres**, snapshotted to memory | Edited via admin API, versioned, read on the hot path from an in-memory snapshot |
| Response cache, rate-limit counters, budget reservations | **Redis** | Shared across instances, tolerant of loss |
| Latency EWMA, circuit-breaker state, in-flight counts | **Per-process**, optionally mirrored to Redis | Hot-path reads must not cross the network |

### Multi-instance consequences, stated plainly

Circuit-breaker state and latency statistics are per-process by default. Behind N replicas, each instance learns about a failing endpoint independently, so an outage is detected up to N times and the effective error-rate threshold is per-instance. This is an accepted trade: shared breaker state on the hot path costs a network round trip on every request to save a handful of failed calls per instance.

What must **not** be per-process: budgets and rate limits. A per-instance budget behind 10 replicas is a 10× budget. These live in Redis with atomic operations, and budgets use a **reservation model** — reserve an estimated maximum before the call, reconcile against actual usage after — because otherwise concurrent requests all read the same "under budget" state and blow straight through the ceiling.

See [ADR-0005](adr/0005-state-store-and-multi-instance.md).

### Configuration snapshots

The catalog and routing policy are read on every request and changed rarely. They are held behind `atomic.Pointer[Catalog]` and replaced wholesale on reload — copy-on-write, never a mutex on the hot path, never a partially-updated catalog. A request that starts under version N completes under version N, and the `Decision` records which version was used, so a past decision can be reproduced exactly.

---

## 8. Token and cost estimation

Pre-request token counts are approximate and the documentation says so rather than pretending otherwise:

- Input tokens require the provider's own tokenizer to be exact. Relay uses a real tokenizer where one is available and a calibrated heuristic otherwise, and marks which was used.
- **Output tokens are unknowable before generation.** Estimates use `max_tokens` as a ceiling and a per-route historical mean as an expectation.

The consequence: estimates are for **admission control and routing** only — is this endpoint plausibly affordable, does this fit the context window. **Budgets, billing, and analytics use actual reported usage**, reconciled after the response completes. Where a provider returns no usage block (some streaming paths), the record is flagged `Estimated: true` and excluded from precision cost reporting rather than silently averaged in.

---

## 9. Observability

**Metrics** (Prometheus). Labelled by tenant, route, endpoint, and outcome — deliberately *not* by model-supplied strings, which are unbounded and will explode cardinality.

- `relay_requests_total{route,endpoint,outcome}`
- `relay_request_duration_seconds` and `relay_gateway_overhead_seconds` (the part that is Relay's fault, measured separately from provider time — without this split you cannot tell whether you are slow or the provider is)
- `relay_ttft_seconds{endpoint}`
- `relay_tokens_total{endpoint,kind}` where kind ∈ input, cached_input, output, reasoning
- `relay_cost_usd_total{tenant,endpoint}`
- `relay_attempts_total{endpoint,error_class}`, `relay_circuit_state{endpoint}`
- `relay_cache_hits_total`, `relay_shed_total`, `relay_streams_active`

**Tracing** (OpenTelemetry). One span per request, child spans for route, each attempt, and each provider call. Attempt spans carry endpoint, error class, and token counts, which makes a failover chain readable at a glance.

**Logging** (Zap, structured, one line per request) with request ID, tenant, route, chosen endpoint, attempt count, tokens, cost, and outcome.

**Prompt and response bodies are never logged by default.** They are the most sensitive data flowing through the system and logging them creates a compliance liability that is far easier to avoid than to unwind. Body capture is opt-in per tenant, subject to secret redaction, and carries a retention policy. `Policy.AllowPromptLogging` gates it.

---

## 10. Concurrency notes (Go)

Small details, each of which has taken down a Go proxy somewhere:

- **One `http.Client` per provider**, with a tuned `Transport`. `MaxIdleConnsPerHost` defaults to **2** — under any real concurrency this silently serializes you into connection churn. Set it to your expected per-host concurrency.
- **Never set `http.Client.Timeout` for streaming.** It bounds the whole response body, so it kills long streams mid-flight. Use per-request `context` deadlines and `Transport`-level `ResponseHeaderTimeout` instead.
- **Propagate `context` everywhere**, and derive the upstream call's context from the inbound request so client disconnect cancels the provider call.
- **No unbounded goroutine spawning per request.** Fan-out is bounded by a semaphore, and every spawned goroutine has a defined exit condition tied to the request context.
- **Metering off the hot path.** Usage and decision records go to a buffered channel drained by a worker pool. When the buffer is full, **drop and increment a counter** — never block a request on telemetry.
- **Prefer atomics and snapshots over locks on the hot path.** Latency EWMA updates use atomics; catalog reads use an atomic pointer swap.
- **`bufio.Scanner`'s 64KB limit** — covered in [§5](#5-streaming), repeated here because it is the bug most likely to be written twice.

---

## 11. Service level objectives

Numbers, so that "is this fast enough" is answerable:

| Objective | Target |
|---|---|
| Gateway overhead (excludes provider time), p50 | < 5 ms |
| Gateway overhead, p99 | < 25 ms |
| Added TTFT overhead on streaming, p99 | < 15 ms |
| Availability of the data plane | 99.9% monthly |
| Concurrent streams per instance | 2,000 |
| Non-streaming throughput per instance | 1,000 rps at the overhead targets above |
| Control plane unavailability impact on data plane | None — serves from last good snapshot |

That last row is a requirement, not an aspiration: if Postgres being down stops inference, the plane separation has failed.

---

## 12. Testing strategy

The architecture is shaped to make these possible, and they are the reason for the shape:

- **Router: exhaustive table tests.** `Route` is pure, so thousands of `(catalog, policy, request) → expected ranking` cases run in milliseconds with no mocks. Injected clock and no randomness; ties break deterministically.
- **Adapters: recorded fixtures + a shared contract suite.** Real provider responses captured once and replayed. Every adapter passes the same suite: streaming and non-streaming, tool calls split across delta boundaries, unicode split across chunk boundaries, empty content, usage reporting, and each error class.
- **Executor: fault injection.** A fake adapter that produces each `ErrorClass` on demand, verifying retry counts, failover order, breaker transitions, deadline enforcement, and the pre/post-first-byte failover boundary.
- **Streaming: leak detection.** `goleak` on every streaming test, plus a soak test asserting that goroutine and FD counts return to baseline after client disconnects mid-stream.
- **End-to-end: mock provider servers** via `httptest`, exercising the full chain including SSE framing.
- **Load: streaming-heavy soak** at the SLO targets, watching overhead percentiles and memory rather than only throughput.
