package optimize

import (
	"fmt"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// anchor is a position in the request prefix where a cache breakpoint could go.
type anchor struct {
	// kind is "system", "tools", or "messages".
	kind string
	// idx is the index of the last element of that block.
	idx int
	// cum is the approximate token count of everything up to and including it.
	cum   int
	label string
}

// placeCacheBreakpoints marks the ends of the request's stable prefixes.
//
// This is the highest-value lever in the package. Providers charge a fraction
// of the normal input price to read a cached prefix, and the saving grows with
// conversation length — precisely when requests are most expensive. It is also
// the lever customers realistically cannot apply themselves, because doing it
// by hand means finding every call site and reasoning about which parts of each
// prompt are stable across turns.
//
// Placement is provider-neutral: which prefix is stable is a fact about the
// request, not about the vendor. Only the encoding of a marker is
// provider-specific, and that stays in the adapter.
func (o *Optimizer) placeCacheBreakpoints(req *domain.NormalizedRequest) *domain.Optimization {
	if !o.cfg.CacheBreakpoints || o.cfg.MaxBreakpoints <= 0 {
		return nil
	}

	anchors := o.anchors(req)
	if len(anchors) == 0 {
		return nil
	}

	// Greedy in prefix order, requiring each breakpoint to add at least
	// MinCacheableTokens over the previous one.
	//
	// The gap rule is what stops two markers landing either side of a trivial
	// message. Every breakpoint costs a cache write, so a marker that saves
	// less than it costs to place is worse than no marker at all.
	var placed []anchor
	lastCum := 0
	for _, a := range anchors {
		if len(placed) >= o.cfg.MaxBreakpoints {
			break
		}
		if a.cum-lastCum < o.cfg.MinCacheableTokens {
			continue
		}
		placed = append(placed, a)
		lastCum = a.cum
	}
	if len(placed) == 0 {
		return nil
	}

	labels := make([]string, 0, len(placed))
	for _, a := range placed {
		if !o.mark(req, a) {
			continue
		}
		labels = append(labels, a.label)
	}
	if len(labels) == 0 {
		return nil
	}

	return &domain.Optimization{
		Lever:  domain.LeverCacheBreakpoints,
		Before: "0",
		After:  fmt.Sprintf("%d", len(labels)),
		Reason: "cacheable prefixes: " + strings.Join(labels, ", "),
	}
}

// anchors lists candidate breakpoint positions in prefix order.
//
// Three, in the order they appear in the prompt, chosen because each is stable
// over a different horizon:
//
//	system + tools  — identical on every request this tenant ever sends
//	history         — identical on the next turn of this conversation
//	full prefix     — becomes the history anchor one turn later
//
// Deliberately not "every message": providers allow only a handful of markers,
// and spreading them evenly through a long conversation would spend the budget
// on prefixes that are re-read once each.
func (o *Optimizer) anchors(req *domain.NormalizedRequest) []anchor {
	var out []anchor
	cum := 0

	for _, p := range req.System {
		cum += p.ApproxTokens()
	}
	toolTokens := 0
	for _, t := range req.Tools {
		toolTokens += t.ApproxTokens()
	}
	cum += toolTokens

	// One anchor for the system+tools preamble, placed on whichever of the two
	// comes last in the wire order the adapters produce.
	switch {
	case len(req.Tools) > 0:
		out = append(out, anchor{kind: "tools", idx: len(req.Tools) - 1, cum: cum, label: "tool definitions"})
	case len(req.System) > 0:
		out = append(out, anchor{kind: "system", idx: len(req.System) - 1, cum: cum, label: "system prompt"})
	}

	if len(req.Messages) == 0 {
		return out
	}

	// History: everything except the final message. On the next turn this
	// prefix is unchanged, so it is the anchor that pays for multi-turn chat.
	historyCum := cum
	for i := 0; i < len(req.Messages)-1; i++ {
		historyCum += req.Messages[i].ApproxTokens()
	}
	if len(req.Messages) > 1 {
		out = append(out, anchor{
			kind: "messages", idx: len(req.Messages) - 2,
			cum: historyCum, label: "conversation history",
		})
	}

	fullCum := historyCum + req.Messages[len(req.Messages)-1].ApproxTokens()
	out = append(out, anchor{
		kind: "messages", idx: len(req.Messages) - 1,
		cum: fullCum, label: "full prefix",
	})

	return out
}

// mark sets the breakpoint flag on the last part of the anchored element.
// Reports false when there is nothing markable there.
func (o *Optimizer) mark(req *domain.NormalizedRequest, a anchor) bool {
	switch a.kind {
	case "system":
		if a.idx < 0 || a.idx >= len(req.System) {
			return false
		}
		req.System[a.idx].CacheBreakpoint = true
		return true
	case "tools":
		if a.idx < 0 || a.idx >= len(req.Tools) {
			return false
		}
		req.Tools[a.idx].CacheBreakpoint = true
		return true
	case "messages":
		if a.idx < 0 || a.idx >= len(req.Messages) {
			return false
		}
		parts := req.Messages[a.idx].Parts
		if len(parts) == 0 {
			return false
		}
		parts[len(parts)-1].CacheBreakpoint = true
		return true
	}
	return false
}
