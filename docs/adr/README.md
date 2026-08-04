# Architecture Decision Records

One decision per file. Each records what was decided, why, and what it costs — including decisions that are still open.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-model-endpoint-as-routing-unit.md) | The routing unit is a model endpoint, not a provider | Accepted |
| [0002](0002-virtual-models-and-routing-contract.md) | Virtual models carry routing intent through the `model` field | Accepted — amended by 0007 |
| [0003](0003-streaming-failover-semantics.md) | Failover is permitted only before the first byte | Accepted |
| [0004](0004-credential-ownership.md) | Who owns provider API keys | **Open** — narrowed to BYOK |
| [0005](0005-state-store-and-multi-instance.md) | Where state lives, and what is per-process | Accepted |
| [0006](0006-classifier-placement.md) | The classifier is a scorer input, not a pipeline stage | Accepted |
| [0007](0007-requested-model-as-baseline.md) | The requested model is a baseline and ceiling, not a pin | Accepted |
| [0008](0008-request-optimization.md) | Request optimization is a component separate from routing | Accepted |
| [0009](0009-quality-floor-and-cascade.md) | Quality is a hard floor, protected by sequential escalation | Accepted |
| [0010](0010-fail-open-availability.md) | Relay fails open to passthrough | Accepted |

**Statuses:** Proposed · Accepted · **Open** (deliberately undecided, with a seam preserved) · Amended · Superseded

An ADR is written when a decision would be expensive to reverse, or when a future reader would otherwise reasonably assume the opposite.

## The four that define the product

0007, 0008, 0009 and 0010 are the ones that turn a gateway into a cost-reduction service, and they are best read as a set. 0007 grants the permission to substitute; 0009 bounds it so substitution cannot degrade output; 0008 adds the savings that carry no quality risk at all; 0010 ensures that none of it can take a customer down.
