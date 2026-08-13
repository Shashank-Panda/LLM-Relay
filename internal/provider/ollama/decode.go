package ollama

import (
	"fmt"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// decodeParts turns an Ollama assistant message into neutral content parts.
func decodeParts(m message, callIndex int) ([]domain.ContentPart, int) {
	var out []domain.ContentPart

	if m.Content != "" {
		out = append(out, domain.ContentPart{Kind: domain.PartText, Text: m.Content})
	}

	for _, tc := range m.ToolCalls {
		out = append(out, domain.ContentPart{
			Kind:       domain.PartToolCall,
			ToolCallID: synthesizeCallID(callIndex),
			ToolName:   tc.Function.Name,
			// Arguments stay as the raw bytes Ollama sent. Every other provider
			// speaks the string form, so this is where the object form is
			// flattened — once, rather than at each consumer.
			Arguments: string(tc.Function.Arguments),
		})
		callIndex++
	}

	return out, callIndex
}

// synthesizeCallID invents the identifier Ollama does not supply.
//
// The OpenAI shape Relay emits requires an ID on every tool call, and clients
// use it to match results back to calls on the next turn. Deriving it from
// position makes it deterministic — the same response always produces the same
// IDs, which is what lets a recorded fixture be asserted exactly.
func synthesizeCallID(i int) string {
	return fmt.Sprintf("call_%d", i)
}

// finishReason maps Ollama's done_reason.
//
// An unrecognised reason maps to stop rather than to empty. A missing
// finish_reason makes SDKs treat the response as truncated and some retry it,
// which turns a cosmetic mapping gap into a doubled bill.
func finishReason(r string, hasToolCalls bool) provider.FinishReason {
	if hasToolCalls {
		return provider.FinishToolCalls
	}
	switch r {
	case "length":
		return provider.FinishLength
	default:
		return provider.FinishStop
	}
}

func usageFrom(r *chatResponse) provider.Usage {
	u := provider.Usage{
		InputTokens:  r.PromptEvalCount,
		OutputTokens: r.EvalCount,
	}
	// Ollama reports counts on the final message. When it does not — an
	// interrupted stream, an older build — the record is flagged rather than
	// left looking like a measured zero.
	u.Estimated = r.PromptEvalCount == 0 && r.EvalCount == 0
	return u
}
