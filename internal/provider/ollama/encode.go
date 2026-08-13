package ollama

import (
	"encoding/json"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// encodeRequest renders a neutral request in Ollama's shape.
func encodeRequest(req *domain.NormalizedRequest, ep *domain.ModelEndpoint, stream bool) *chatRequest {
	out := &chatRequest{
		Model:  ep.Model,
		Stream: stream,
	}

	// Ollama takes the system prompt as a message, so the hoisting the wire
	// decoder did is undone here. Rendering it back is cheap; re-deriving which
	// messages were system ones in three adapters is not.
	if len(req.System) > 0 {
		if text := joinText(req.System); text != "" {
			out.Messages = append(out.Messages, message{Role: string(domain.RoleSystem), Content: text})
		}
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

	// Ollama's structured output takes the schema itself, or the literal string
	// "json" for free-form JSON.
	switch req.ResponseFormat.Type {
	case domain.FormatJSONSchema:
		if req.ResponseFormat.Schema != "" {
			out.Format = json.RawMessage(req.ResponseFormat.Schema)
		}
	case domain.FormatJSONObject:
		out.Format = "json"
	}

	opts := &options{
		Temperature: req.Params.Temperature,
		TopP:        req.Params.TopP,
		Seed:        req.Params.Seed,
		NumPredict:  req.Params.MaxTokens,
		Stop:        req.Params.Stop,
	}
	if !opts.empty() {
		out.Options = opts
	}

	// Reasoning effort is deliberately dropped. Ollama has no equivalent, and
	// the endpoint does not declare the reasoning capability — so a caller-set
	// effort would have eliminated it during filtering and anything that
	// reaches here is an optimizer-applied default, which by contract must not
	// change what the model is asked.

	return out
}

// encodeMessage renders one neutral message, splitting it where Ollama needs
// separate messages.
//
// A single assistant turn containing several tool results becomes several
// messages, because Ollama pairs a result with its call by carrying one name
// per message. Flattening them into one would lose the pairing, and a tool
// result whose call cannot be identified is one the model cannot use.
func encodeMessage(m domain.Message) []message {
	var (
		out     []message
		text    strings.Builder
		images  []string
		calls   []toolCall
		results []message
	)

	for _, p := range m.Parts {
		switch p.Kind {
		case domain.PartText:
			text.WriteString(p.Text)
		case domain.PartImage:
			// Ollama takes bare base64, no data URI wrapper. A remote URL it
			// cannot fetch, so one is skipped rather than sent to fail.
			if p.Data != "" {
				images = append(images, p.Data)
			}
		case domain.PartToolCall:
			calls = append(calls, toolCall{Function: toolCallFunction{
				Name:      p.ToolName,
				Arguments: rawOrNil(p.Arguments),
			}})
		case domain.PartToolResult:
			results = append(results, message{
				Role:     string(domain.RoleTool),
				Content:  p.Text,
				ToolName: p.ToolName,
			})
		case domain.PartReasoning:
			// Reasoning is not echoed back. Providers that require it say so;
			// Ollama does not, and replaying it would be billed as input.
		}
	}

	if len(results) > 0 {
		return results
	}

	if text.Len() > 0 || len(images) > 0 || len(calls) > 0 {
		out = append(out, message{
			Role:      string(m.Role),
			Content:   text.String(),
			Images:    images,
			ToolCalls: calls,
		})
	}
	return out
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

// rawOrNil avoids emitting the four bytes "null" for an absent schema, which
// some providers treat as an explicitly empty schema rather than no schema.
func rawOrNil(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return json.RawMessage(s)
}
