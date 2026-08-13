package wire

import (
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

const (
	objectCompletion = "chat.completion"
	objectChunk      = "chat.completion.chunk"
	roleAssistant    = "assistant"
)

// EncodeResponse renders a completed answer in the OpenAI shape.
//
// Model is the name the caller asked for, not the endpoint that served it.
// Clients key caches, dashboards, and occasionally control flow on this field,
// and substituting the served model would break them silently. When Relay does
// serve something different, that fact is disclosed in headers where it can be
// read deliberately rather than tripped over.
func EncodeResponse(resp *provider.Response, requestedModel string, created int64) *ChatResponse {
	out := &ChatResponse{
		ID:      resp.ID,
		Object:  objectCompletion,
		Created: created,
		Model:   requestedModel,
	}
	if out.ID == "" {
		out.ID = "chatcmpl-relay"
	}

	msg := &ChoiceMessage{Role: roleAssistant}

	var text, reasoning strings.Builder
	for _, p := range resp.Parts {
		switch p.Kind {
		case domain.PartText:
			text.WriteString(p.Text)
		case domain.PartReasoning:
			reasoning.WriteString(p.Text)
		case domain.PartToolCall:
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:       p.ToolCallID,
				Type:     "function",
				Function: FunctionCall{Name: p.ToolName, Arguments: p.Arguments},
			})
		}
	}

	// Content is *string, and the distinction is load-bearing: a pure tool-call
	// response carries content: null, and SDKs branch on exactly that. Emitting
	// "" instead makes a tool call look like an empty answer.
	if s := text.String(); s != "" || len(msg.ToolCalls) == 0 {
		msg.Content = &s
	}
	msg.Reasoning = reasoning.String()

	finish := string(resp.FinishReason)
	if finish == "" {
		finish = string(provider.FinishStop)
	}

	out.Choices = []Choice{{Index: 0, Message: msg, FinishReason: &finish}}
	out.Usage = encodeUsage(resp.Usage)
	return out
}

// EncodeChunk renders one streaming delta.
func EncodeChunk(c *provider.Chunk, id, requestedModel string, created int64) *ChatChunk {
	delta := &Delta{}

	if c.Text != "" {
		s := c.Text
		delta.Content = &s
	}
	if c.Reasoning != "" {
		delta.Reasoning = c.Reasoning
	}
	if tc := c.ToolCall; tc != nil {
		idx := tc.Index
		call := ToolCall{Index: &idx, Function: FunctionCall{Arguments: tc.Arguments}}
		// ID, type, and name appear only on the opening delta for an index.
		// Repeating them on every fragment is a shape some SDKs mis-assemble,
		// and it is not what the providers do.
		if tc.ID != "" || tc.Name != "" {
			call.ID = tc.ID
			call.Type = "function"
			call.Function.Name = tc.Name
		}
		delta.ToolCalls = []ToolCall{call}
	}

	choice := Choice{Index: 0, Delta: delta}
	if c.FinishReason != "" {
		f := string(c.FinishReason)
		choice.FinishReason = &f
	}

	return &ChatChunk{
		ID:      id,
		Object:  objectChunk,
		Created: created,
		Model:   requestedModel,
		Choices: []Choice{choice},
	}
}

// RoleChunk is the opening frame.
//
// OpenAI sends a chunk carrying only role:"assistant" before any content, and
// SDKs that build a message incrementally rely on it to initialise the object.
// Omitting it produces deltas applied to nothing.
func RoleChunk(id, requestedModel string, created int64) *ChatChunk {
	return &ChatChunk{
		ID:      id,
		Object:  objectChunk,
		Created: created,
		Model:   requestedModel,
		Choices: []Choice{{Index: 0, Delta: &Delta{Role: roleAssistant}}},
	}
}

// UsageChunk is the final frame before [DONE].
//
// Choices is an empty array rather than absent — that is the shape OpenAI emits
// and therefore the one SDKs parse without complaint.
func UsageChunk(u provider.Usage, id, requestedModel string, created int64) *ChatChunk {
	return &ChatChunk{
		ID:      id,
		Object:  objectChunk,
		Created: created,
		Model:   requestedModel,
		Choices: []Choice{},
		Usage:   encodeUsage(u),
	}
}

func encodeUsage(u provider.Usage) *Usage {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	out := &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.CachedInputTokens > 0 {
		out.PromptTokensDetails = &PromptTokensDetails{CachedTokens: u.CachedInputTokens}
	}
	if u.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &CompletionTokensDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// EncodeModels renders GET /v1/models from a catalog snapshot.
//
// Aliases and route names are listed alongside endpoint IDs, because all three
// are things a caller may legitimately put in the model field. A list that
// omitted the virtual models would make Relay's own routes look unavailable to
// anything that checks the list before calling.
func EncodeModels(cat *domain.Catalog, created int64) *ModelList {
	out := &ModelList{Object: "list", Data: []Model{}}
	if cat == nil {
		return out
	}

	for _, id := range domain.SortedKeys(cat.Endpoints) {
		ep := cat.Endpoints[id]
		if ep.Lifecycle.Status == domain.StatusRetired {
			continue
		}
		out.Data = append(out.Data, Model{
			ID: id, Object: "model", Created: created, OwnedBy: string(ep.Provider),
		})
	}
	for _, alias := range domain.SortedKeys(cat.Aliases) {
		owner := "relay"
		if ep, ok := cat.Endpoint(cat.Aliases[alias]); ok {
			owner = string(ep.Provider)
		}
		out.Data = append(out.Data, Model{
			ID: alias, Object: "model", Created: created, OwnedBy: owner,
		})
	}
	for _, name := range domain.SortedKeys(cat.Routes) {
		out.Data = append(out.Data, Model{
			ID: name, Object: "model", Created: created, OwnedBy: "relay",
		})
	}
	return out
}
