# Architecture Decision Records

One decision per file. Each records what was decided, why, and what it costs — including decisions that are still open.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-model-endpoint-as-routing-unit.md) | The routing unit is a model endpoint, not a provider | Accepted |
| [0002](0002-virtual-models-and-routing-contract.md) | Virtual models carry routing intent through the `model` field | Accepted |
| [0003](0003-streaming-failover-semantics.md) | Failover is permitted only before the first byte | Accepted |
| [0004](0004-credential-ownership.md) | Who owns provider API keys | **Open** |
| [0005](0005-state-store-and-multi-instance.md) | Where state lives, and what is per-process | Accepted |
| [0006](0006-classifier-placement.md) | The classifier is a scorer input, not a pipeline stage | Accepted |

**Statuses:** Proposed · Accepted · **Open** (deliberately undecided, with a seam preserved) · Superseded

An ADR is written when a decision would be expensive to reverse, or when a future reader would otherwise reasonably assume the opposite.
