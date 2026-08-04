# ADR-0003 — Failover is permitted only before the first byte

**Status:** Accepted
**Date:** 2026-08-04

## Context

Relay promises two things that conflict: transparent failover between providers, and token streaming.

They are compatible right up until the first chunk reaches the client. After that, the client has committed — it has parsed a chunk, likely rendered text to a user, and possibly begun acting on partial content. If the upstream provider then fails at 60% of the way through a response, there is no way to switch providers and continue that is honest. The alternatives are all bad in different ways: restarting produces duplicated text, splicing produces a response no single model would have generated, and pretending the failure did not happen is not available.

Provider failures do occur mid-stream — connection resets, upstream 5xx after headers, rate limits applied late, content filters triggering partway through generation.

## Decision

**Before the first chunk is flushed to the client:** Relay may abandon an attempt and try the next ranked candidate. This is fully transparent; the client sees one clean stream and the failover appears only in the decision record and metrics.

**After the first chunk is flushed:** no failover. The stream terminates with an SSE error event carrying the error class and the endpoint that failed, followed by stream close. Partial content already delivered stays delivered.

The `Attempt` record captures whether TTFT was reached, so the two cases are distinguishable in analytics and the post-first-byte failure rate can be monitored as its own number.

Non-streaming requests are unaffected — the entire response is buffered, so failover is available for the whole call.

## Consequences

**Good.** Every stream is honest: what the client received is what one model actually produced, contiguously. TTFT is not penalized — nothing is held back waiting for a failure that will usually not come. The implementation is simple, and simple is valuable in the code path most likely to leak goroutines.

**Costs.** A real class of failure is not covered. Mid-stream failures surface to the client, which must handle a partial response — so the error contract must be documented clearly for client authors, and the SSE error event shape is part of the public API.

**Mitigations.** Pre-first-byte failover covers the most common failures (connection refused, immediate 429, immediate 5xx), because most provider failures happen before generation starts. Circuit breaking removes known-bad endpoints from the candidate list before they are ever attempted. And `relay_stream_failures_after_ttft_total` is monitored specifically, so if this gap turns out to be larger in practice than expected, the decision gets revisited with data instead of intuition.

## Alternatives considered

**Buffer until first token, then release.** Would allow failover through the highest-risk window. Rejected: it adds the full time-to-first-token to every streaming request in order to protect an uncommon case — paying a certain latency cost on 100% of requests to insure against a small fraction. Streaming exists precisely to reduce perceived latency; this trades away the feature to protect the feature.

**Restart the stream on a new endpoint and re-emit from the beginning.** Rejected: clients that appended the earlier chunks now show duplicated text, and there is no mechanism in the OpenAI SSE protocol to instruct a client to discard what it has already received.

**Splice: continue generation on a second model from the partial text.** Rejected: produces output no single model generated, with a discontinuity in voice and reasoning at the seam, and makes cost attribution and evaluation meaningless.
