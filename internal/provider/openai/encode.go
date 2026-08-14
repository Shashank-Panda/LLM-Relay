package openai

import (
	"encoding/json"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

func encodeRequest(req *domain.NormalizedRequest, ep *domain.ModelEndpoint, stream bool) *chatRequest {
	out := &chatRequest{
		Model:  ep.Model,
		Stream: stream,

		Temperature: req.Params.Temperature,
		TopP:        req.Params.TopP,
		MaxTokens:   req.Params.MaxTokens,
		Seed:        req.Params.Seed,
		Stop:        req.Params.Stop,
	}

	if stream {
		// Without this OpenAI streams no usage block at all, every streamed
		// request records Estimated usage, and cost reporting loses the
		// majority of traffic.
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	// OpenAI takes the system prompt as a message. The role name depends on the
	// model family — reasoning models want "developer" — but "system" is still
	// accepted everywhere, so one spelling avoids a per-model lookup that would
	// have to be maintained against a moving target.
	if text := joinText(req.System); text != "" {
		out.Messages = append(out.Messages, message{
			Role:    string(domain.RoleSystem),
			Content: mustJSON(text),
		})
	}

	for _, m := range req.Messages {
		out.Messages = append(out.Messages, encodeMessage(m)...)
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, tool{
			Type: "function",
			Function: toolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  rawOrNil(t.Schema),
			},
		})
	}

	if tc := req.ToolChoice; tc != "" {
		switch tc {
		case "auto", "none", "required":
			out.ToolChoice = tc
		default:
			out.ToolChoice = map[string]any{
				"type":     "function",
				"function": map[string]string{"name": tc},
			}
		}
	}

	switch req.ResponseFormat.Type {
	case domain.FormatJSONObject:
		out.ResponseFormat = &responseFormat{Type: "json_object"}
	case domain.FormatJSONSchema:
		out.ResponseFormat = &responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchema{
				Name:   "response",
				Schema: rawOrNil(req.ResponseFormat.Schema),
				Strict: true,
			},
		}
	}

	// Effort is sent only where the endpoint declares reasoning support.
	//
	// A caller-set effort could not have reached a non-reasoning endpoint — it
	// is a hard capability requirement and the filter would have eliminated it.
	// An optimizer-applied default can, and forwarding it to a model that does
	// not understand the parameter turns a working request into a 400. The
	// optimizer's contract is that its defaults never change the outcome, so
	// dropping it here is what keeps that promise.
	if e := req.Params.ReasoningEffort; e != nil && ep.Capabilities.Reasoning {
		out.ReasoningEffort = string(*e)
	}

	return out
}

// encodeMessage renders one neutral message.
//
// Tool results become their own messages with role "tool", because OpenAI
// models the pairing that way. A turn holding several results becomes several
// messages; merging them would lose the call IDs, and OpenAI rejects a result
// it cannot pair with a call.
func encodeMessage(m domain.Message) []message {
	var (
		results []message
		text    strings.Builder
		parts   []contentPart
		calls   []toolCall
		hasRich bool
	)

	for _, p := range m.Parts {
		switch p.Kind {
		case domain.PartText:
			text.WriteString(p.Text)
			parts = append(parts, contentPart{Type: "text", Text: p.Text})
		case domain.PartImage:
			hasRich = true
			parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: imageRef(p)}})
		case domain.PartToolCall:
			calls = append(calls, toolCall{
				ID:       p.ToolCallID,
				Type:     "function",
				Function: functionCall{Name: p.ToolName, Arguments: p.Arguments},
			})
		case domain.PartToolResult:
			results = append(results, message{
				Role:       string(domain.RoleTool),
				ToolCallID: p.ToolCallID,
				Content:    mustJSON(p.Text),
			})
		case domain.PartReasoning:
			// Not echoed. OpenAI does not require it and it would be billed as
			// input on every subsequent turn.
		}
	}

	if len(results) > 0 {
		return results
	}

	out := message{Role: string(m.Role), Name: m.Name, ToolCalls: calls}

	switch {
	case hasRich:
		// The array form is only needed for multimodal content. Using it
		// everywhere is legal but noisier on the wire and rejected by some
		// OpenAI-compatible servers that only implement the string form.
		out.Content = mustJSON(parts)
	case text.Len() > 0:
		out.Content = mustJSON(text.String())
	case len(calls) > 0:
		// A pure tool-call turn carries content:null, which is the shape
		// OpenAI itself emits and expects back.
		out.Content = json.RawMessage("null")
	default:
		return nil
	}

	return []message{out}
}

// imageRef reassembles a data URI, or passes a remote URL through.
func imageRef(p domain.ContentPart) string {
	if p.Data == "" {
		return p.URL
	}
	mt := p.MediaType
	if mt == "" {
		mt = "image/png"
	}
	return "data:" + mt + ";base64," + p.Data
}

func joinText(parts []domain.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == domain.PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func rawOrNil(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return json.RawMessage(s)
}

// mustJSON encodes a value that cannot fail to encode: strings and slices of
// plain structs. An error here would mean the domain model holds something
// unrepresentable, which is a programming error, not a runtime condition.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return b
}
