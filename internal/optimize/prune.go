package optimize

import (
	"fmt"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// pruneContext drops the oldest turns beyond a retention window.
//
// This is the one lever that is not semantics-preserving. The other three leave
// the model asked exactly the same question; this one deletes part of it. The
// model loses history it might have used, and no amount of care makes that
// invisible — which is why it is off by default and requires its own opt-in
// rather than riding along with the safe levers.
//
// System prompt and tool definitions are never pruned. They are the request's
// instructions, not its history.
func (o *Optimizer) pruneContext(req *domain.NormalizedRequest) *domain.Optimization {
	if !o.cfg.ContextPruning || o.cfg.KeepTurns <= 0 {
		return nil
	}
	if len(req.Messages) <= o.cfg.KeepTurns {
		return nil
	}

	cut := len(req.Messages) - o.cfg.KeepTurns

	// Advance the cut past any leading tool results.
	//
	// A tool result whose originating tool call was dropped is an orphan, and
	// providers reject it — OpenAI with a 400 for a tool message with no
	// preceding call, Anthropic likewise for an unmatched tool_result block. So
	// pruning that ignores pairing does not save money; it turns a working
	// expensive request into a failing one.
	for cut < len(req.Messages) && startsWithOrphanedResult(req.Messages[cut]) {
		cut++
	}

	// Never prune everything: the final message is the request itself.
	if cut >= len(req.Messages) {
		return nil
	}
	if cut <= 0 {
		return nil
	}

	before := req.InputTokens()
	req.Messages = req.Messages[cut:]
	after := req.InputTokens()

	return &domain.Optimization{
		Lever:  domain.LeverContextPrune,
		Before: fmt.Sprintf("%d messages, ~%d tokens", cut+len(req.Messages), before),
		After:  fmt.Sprintf("%d messages, ~%d tokens", len(req.Messages), after),
		Reason: fmt.Sprintf("retention window is %d turns", o.cfg.KeepTurns),
	}
}

func startsWithOrphanedResult(m domain.Message) bool {
	return m.Role == domain.RoleTool || m.HasKind(domain.PartToolResult)
}
