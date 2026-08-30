# ADR-0011 — Relay is licensed Apache-2.0

**Status:** Accepted
**Date:** 2026-08-19

## Context

The README carried "License: Not yet chosen" from the first commit. That is a blocking answer for
the two deployments the project already promises. A hosted customer routing production traffic
through a vendor asks what the terms are; a self-hosted enterprise install cannot begin without
one, because an unlicensed repository grants no rights at all — the default is exclusive copyright,
not permissiveness. Every step toward "a stranger can run this" runs through this decision, so it
is made here rather than deferred again.

## Options

### Apache-2.0

- **Expresses a patent grant (§3), with defensive termination.** Relay asks to be a hard dependency
  in someone's critical path. That is the review where patent exposure gets asked about, and it is
  the single largest adoption difference between the candidates.
- **§5 licenses inbound contributions on the same terms**, so the common case needs no separate CLA.
- The norm for Go infrastructure, which means it reads as unremarkable to a reviewer — a property
  worth more than it sounds.
- Costs: longer than MIT, and requires retaining notices in derivative works.

### MIT

Shorter, universally understood, and permissive in the same practical ways. **Silent on patents.**
For a library that would be fine. For a component a company is asked to place between its
applications and its model providers, silence is the weaker answer to the question that will
actually be asked.

### AGPL-3.0

The instinct here is to stop a cloud vendor reselling the hosted product. It is the wrong tool for
this codebase, because it attacks the wrong half: `README` offers **self-hosted deployment in the
customer's own VPC** as a first-class option, and a large share of enterprises operate blanket AGPL
bans that would make that offer unusable. It defends the business model by disabling the go-to-market.

### BSL / Elastic License

Protects the hosted business most directly and is **not OSI-approved**. That kills the "clone it and
run it" motion, which is the entire purpose of the work this ADR is part of. Reconsider only if
hosted revenue exists and is being competed away — a problem worth having and not one yet.

## Decision

**Apache-2.0.** The patent grant is decisive for a product whose stated deployment model is
"put this in your critical path", and the contribution terms are a real convenience at no cost.

## Consequences

- `LICENSE` at the repository root; the README's License section points here rather than restating.
- Relicensing later requires the agreement of every contributor, so this is expensive to reverse —
  which is why it is an ADR and not a README edit.
- No `NOTICE` file is added yet. One becomes necessary only when third-party Apache-licensed source
  is vendored; the current dependencies are consumed as modules, not copied.
- This says nothing about the trademark or the hosted service's terms of service. Those are
  separate instruments and the license deliberately does not reach them (§6).
