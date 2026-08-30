# ADR-0012 — The console is a separate Next.js app that proxies to Relay

**Status:** Accepted
**Date:** 2026-08-19

## Context

[The roadmap](../roadmap.md) puts "Frontend dashboard" under *Explicitly out of scope*, with a
correct reason: Grafana over Prometheus covers operator needs, and building a dashboard instead of
the gateway would have been the wrong order. That cut is amended rather than reversed, because what
gets built here is not the thing that was cut.

The observation that reopens it: **Relay already computes its own sales argument on every request
and has no way to show it.** `Decision` carries the ranked candidates, each dimension's weighted
contribution, every rejection with its reason, and both costs. `X-Relay-Dry-Run` already serializes
all of it — with no provider call, no credential, no charge, and no ledger record. The product's
central claim is therefore already a JSON document that anybody can obtain for free, and the only
thing standing between it and a prospective user is that reading it requires `curl` and `jq`.

That is not an operator dashboard. It is the adoption surface, and it cannot be outsourced to a
Grafana link for the same reason the roadmap already carved out the Phase 6 savings report: it is
the proof, and the proof is the product.

## Decision

Build the console as a **separate Next.js application in `web/`**, and route **every browser request
through its own server-side handlers** rather than letting the browser reach Relay directly.

## What the separate app costs

Recorded plainly, because it is a real loss and a future reader should not have to rediscover it.

Relay today is one static CGO-free binary plus two YAML files. That is why the image can be
distroless, why CI is `go build`, why the dependency list is three modules, and why "run the same
binary in your own VPC" is a five-minute promise rather than a project. A Next console adds a Node
toolchain to the build, a second container to every deployment, a dependency surface orders of
magnitude larger than `go.mod`, a second CI job, and a second process that can be down while Relay
is up.

`go:embed` of a built static bundle would have kept the single-binary property while still allowing
React. It was not chosen. **That door stays open at low cost provided `web/` is built as a static
export**, and it should be reconsidered if the console ever becomes something a self-hoster is
expected to run rather than something they may.

The compensating constraint: **the console is never a dependency of the gateway.** It is an optional
compose service. Relay must build, test, ship, and serve traffic with `web/` deleted.

## Why the browser never talks to Relay directly

Four consequences, and the second is the one that makes it non-negotiable.

1. **No CORS middleware in Go.** Relay is meant to be callable by any SDK in any language, and the
   only honest allowed-origin default for such a service is "no policy". Same-origin proxying
   deletes the question instead of answering it badly.
2. **The admin listener stays off the internet.** The savings report lives on the control-plane
   listener, which is loopback-bound precisely because it has *no authentication until Phase 7* and
   returns every tenant's spend when unscoped. A browser must not be able to reach it. The console's
   server can, over a private network, and it is the only component in the system that does.
3. **One place to rate-limit and audit** console traffic, separate from Relay's own admission control.
4. **In a hosted deployment the Relay tenant key stays server-side**, so the browser holds only the
   user's own provider key.

## Consequences

- The proxy is a trust boundary: a caller-supplied provider key passes through the console's server
  process. In a self-hosted install both processes belong to the same operator and this costs
  nothing. In a hosted demo it is a real delegation and **must be disclosed in the UI**, not only in
  documentation. The alternative — a browser talking to Relay directly — trades that disclosure for
  a public CORS policy and a publicly reachable gateway port, which is worse on both counts.
- Console handlers must not log request bodies or headers, mirroring the access log's existing
  restraint.
- Relay's dry-run response becomes a published interface with a second consumer in another language.
  A Go-generated golden fixture pins it, so a changed struct tag fails a Go test rather than
  rendering `undefined` in a browser.
- The roadmap's out-of-scope entry is amended in place rather than deleted, per that file's own rule
  that cuts are recorded so the reasons can be revisited rather than re-argued.
