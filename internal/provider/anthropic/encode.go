package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// defaultMaxTokens is used when neither the caller nor the optimizer set a
// ceiling. Anthropic requires max_tokens, so some number is mandatory; the
// endpoint's own declared limit is the least surprising one.
const defaultMaxTokens = 4096

func encodeRequest(req *domain.NormalizedRequest, ep *domain.ModelEndpoint, stream bool) *messagesRequest {
	out := &messagesRequest{
		Model:  ep.Model,
		Stream: stream,

		Temperature:   req.Params.Temperature,
		TopP:          req.Params.TopP,
		StopSequences: req.Params.Stop,
		MaxTokens:     maxTokens(req, ep),
	}

	// The system prompt is a top-level field here, not a message. This is the
	// clearest case for hoisting it during normalization: an adapter that had
	// to scan the message list for system turns would also have to decide what
	// to do with one that appeared in the middle.
	for _, p := range req.System {
		if p.Kind == domain.PartText && p.Text != "" {
			out.System = append(out.System, textBlock(p))
		}
	}

	out.Messages = encodeMessages(req.Messages)

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, tool{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  rawOrNil(t.Schema),
			CacheControl: cacheControlFor(t.CacheBreakpoint),
		})
	}

	if tc := req.ToolChoice; tc != "" {
		switch tc {
		case "auto":
			out.ToolChoice = &toolChoice{Type: "auto"}
		case "required":
			out.ToolChoice = &toolChoice{Type: "any"}
		case "none":
			out.ToolChoice = &toolChoice{Type: "none"}
		default:
			out.ToolChoice = &toolChoice{Type: "tool", Name: tc}
		}
	}

	// Anthropic has no json_schema response format. Structured output is
	// expressed as a tool the model is forced to call, and building that
	// translation belongs in Phase 3 alongside the rest of the optimizer work
	// rather than being half-done here. Until then the request is sent without
	// it: the model usually complies with a schema stated in the prompt, and
	// silently dropping the constraint is better than silently inventing a tool
	// the caller never declared.

	if th := encodeThinking(req, ep); th != nil {
		out.Thinking = th
		// Extended thinking requires temperature to be unset. Sending both is a
		// 400, so the caller's temperature is dropped rather than the reasoning
		// they explicitly asked for.
		out.Temperature = nil
		out.TopP = nil
	}

	return out
}

// maxTokens resolves the required ceiling.
func maxTokens(req *domain.NormalizedRequest, ep *domain.ModelEndpoint) int {
	n := defaultMaxTokens
	switch {
	case req.Params.MaxTokens != nil && *req.Params.MaxTokens > 0:
		n = *req.Params.MaxTokens
	case req.Estimate.MaxOutputTokens > 0:
		n = req.Estimate.MaxOutputTokens
	}
	// Anthropic rejects a ceiling above the model's own limit, so a caller who
	// asks for more than the endpoint can produce gets the endpoint's maximum
	// rather than a 400.
	if lim := ep.Limits.MaxOutputTokens; lim > 0 && n > lim {
		n = lim
	}
	return n
}

// encodeThinking maps a neutral effort level onto a token budget.
//
// Anthropic expresses reasoning as a budget rather than a level, so the mapping
// is a judgement call rather than a translation. The budgets below are the ones
// Anthropic's own documentation uses as illustrative tiers.
func encodeThinking(req *domain.NormalizedRequest, ep *domain.ModelEndpoint) *thinking {
	e := req.Params.ReasoningEffort
	if e == nil || !ep.Capabilities.Reasoning {
		return nil
	}

	budget := 0
	switch *e {
	case domain.EffortMinimal:
		// Below Anthropic's 1024-token floor, thinking cannot be enabled at
		// all. "Minimal" is honoured as "off" rather than rounded up into a
		// budget the caller did not ask for.
		return nil
	case domain.EffortLow:
		budget = 2048
	case domain.EffortMedium:
		budget = 8192
	case domain.EffortHigh:
		budget = 16384
	default:
		return nil
	}

	// The budget must leave room for the answer itself within max_tokens.
	if limit := maxTokens(req, ep) - 1024; budget > limit {
		budget = limit
	}
	if budget < 1024 {
		return nil
	}

	return &thinking{Type: "enabled", BudgetTokens: budget}
}

// encodeMessages renders the neutral turns into Anthropic's shape.
//
// The awkward part: a tool *result* belongs in a user message here, not in a
// message of its own with a tool role. So a neutral tool turn becomes a user
// message full of tool_result blocks, and consecutive same-role messages are
// merged because Anthropic rejects a conversation that alternates incorrectly.
func encodeMessages(msgs []domain.Message) []message {
	var out []message

	appendBlocks := func(role string, blocks []block) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, message{Role: role, Content: blocks})
	}

	for _, m := range msgs {
		role := string(m.Role)
		var blocks []block

		for _, p := range m.Parts {
			switch p.Kind {
			case domain.PartText:
				if p.Text != "" {
					blocks = append(blocks, textBlock(p))
				}
			case domain.PartImage:
				blocks = append(blocks, imageBlock(p))
			case domain.PartToolCall:
				blocks = append(blocks, block{
					Type:         "tool_use",
					ID:           p.ToolCallID,
					Name:         p.ToolName,
					Input:        inputOrEmpty(p.Arguments),
					CacheControl: cacheControlFor(p.CacheBreakpoint),
				})
			case domain.PartToolResult:
				// A tool result is a user-role block. This is the single
				// biggest structural difference from the OpenAI shape, and
				// getting it wrong produces a 400 on every tool-using turn.
				role = string(domain.RoleUser)
				blocks = append(blocks, block{
					Type:      "tool_result",
					ToolUseID: p.ToolCallID,
					Content:   p.Text,
				})
			case domain.PartReasoning:
				// Thinking blocks are not echoed back. Anthropic requires the
				// original signature to accept one, and a block replayed
				// without it is rejected.
			}
		}

		if role == string(domain.RoleTool) {
			role = string(domain.RoleUser)
		}
		appendBlocks(role, blocks)
	}

	return out
}

func textBlock(p domain.ContentPart) block {
	return block{Type: "text", Text: p.Text, CacheControl: cacheControlFor(p.CacheBreakpoint)}
}

func imageBlock(p domain.ContentPart) block {
	if p.Data != "" {
		mt := p.MediaType
		if mt == "" {
			mt = "image/png"
		}
		return block{Type: "image", Source: &imageSource{
			Type: "base64", MediaType: mt, Data: p.Data,
		}}
	}
	return block{Type: "image", Source: &imageSource{Type: "url", URL: p.URL}}
}

// cacheControlFor is the encoding half of prompt caching. Placement is the
// optimizer's decision — a provider-neutral fact about the request's stable
// prefix — and only this spelling is Anthropic's.
func cacheControlFor(mark bool) *cacheControl {
	if !mark {
		return nil
	}
	return &cacheControl{Type: "ephemeral"}
}

func rawOrNil(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return json.RawMessage(s)
}

// inputOrEmpty renders tool arguments.
//
// Anthropic requires input to be an object, and a tool call with no arguments
// arrives from other providers as an empty string. Sending that verbatim is a
// 400, so the empty case becomes {}.
func inputOrEmpty(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}
