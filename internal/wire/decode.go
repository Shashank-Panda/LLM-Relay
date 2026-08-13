package wire

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// Stop is the stop-sequence field, which OpenAI accepts as either a string or
// an array of strings. Both forms are in wide use by real SDKs.
type Stop []string

func (s *Stop) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = Stop{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = Stop(many)
	return nil
}

func (s Stop) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return []byte("null"), nil
	}
	return json.Marshal([]string(s))
}

// DecodeError is a request Relay will not attempt. It carries the field so the
// caller can fix it without guessing.
type DecodeError struct {
	Field   string
	Message string
}

func (e *DecodeError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// Decode converts a wire request into the provider-neutral form.
//
// Validation is strict about what would produce a wrong answer and permissive
// about everything else. An unknown tool_choice string is passed through — some
// provider may understand it — but a message with no role is rejected, because
// there is no safe guess and every downstream component would have to invent
// one independently.
func Decode(req *ChatRequest) (*domain.NormalizedRequest, error) {
	if req == nil {
		return nil, &DecodeError{Message: "empty request body"}
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, &DecodeError{Field: "model", Message: "is required"}
	}
	if len(req.Messages) == 0 {
		return nil, &DecodeError{Field: "messages", Message: "must contain at least one message"}
	}

	out := &domain.NormalizedRequest{
		Stream:   req.Stream,
		Metadata: req.Metadata,
	}

	for i, m := range req.Messages {
		field := fmt.Sprintf("messages[%d]", i)

		role := domain.Role(m.Role)
		switch role {
		case domain.RoleSystem, domain.RoleUser, domain.RoleAssistant, domain.RoleTool:
		case "developer":
			// OpenAI renamed the system role for reasoning models. Same meaning,
			// and callers mix the two freely.
			role = domain.RoleSystem
		case "":
			return nil, &DecodeError{Field: field + ".role", Message: "is required"}
		default:
			return nil, &DecodeError{
				Field:   field + ".role",
				Message: fmt.Sprintf("%q is not one of system, developer, user, assistant, tool", m.Role),
			}
		}

		parts, err := decodeContent(m.Content, field)
		if err != nil {
			return nil, err
		}

		// A tool result is a content part in the neutral model, not a bare
		// message body. Keeping the call ID attached is what lets adapters
		// re-pair results with calls for providers that require the pairing.
		if role == domain.RoleTool {
			for j := range parts {
				parts[j].Kind = domain.PartToolResult
				parts[j].ToolCallID = m.ToolCallID
			}
			if len(parts) == 0 {
				parts = []domain.ContentPart{{Kind: domain.PartToolResult, ToolCallID: m.ToolCallID}}
			}
		}

		for _, tc := range m.ToolCalls {
			parts = append(parts, domain.ContentPart{
				Kind:       domain.PartToolCall,
				ToolCallID: tc.ID,
				ToolName:   tc.Function.Name,
				Arguments:  tc.Function.Arguments,
			})
		}

		// The system prompt is hoisted out of the message list. Some providers
		// take it as a top-level field and some as a message; making it a
		// distinct field means each adapter renders it its own way instead of
		// every adapter re-discovering which messages were system ones.
		if role == domain.RoleSystem {
			out.System = append(out.System, parts...)
			continue
		}

		out.Messages = append(out.Messages, domain.Message{
			Role:  role,
			Name:  m.Name,
			Parts: parts,
		})
	}

	if len(out.Messages) == 0 {
		return nil, &DecodeError{
			Field:   "messages",
			Message: "must contain at least one non-system message",
		}
	}

	for i, t := range req.Tools {
		if t.Function.Name == "" {
			return nil, &DecodeError{
				Field:   fmt.Sprintf("tools[%d].function.name", i),
				Message: "is required",
			}
		}
		out.Tools = append(out.Tools, domain.ToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Schema:      string(t.Function.Parameters),
		})
	}

	out.ToolChoice = decodeToolChoice(req.ToolChoice)

	if rf := req.ResponseFormat; rf != nil {
		out.ResponseFormat = domain.ResponseFormat{Type: domain.ResponseFormatType(rf.Type)}
		if rf.JSONSchema != nil {
			out.ResponseFormat.Schema = string(rf.JSONSchema.Schema)
		}
	}

	out.Params = domain.SamplingParams{
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Seed:        req.Seed,
		Stop:        []string(req.Stop),
	}

	// max_completion_tokens is the reasoning-model spelling of max_tokens.
	// Whichever arrived is the caller's ceiling.
	switch {
	case req.MaxTokens != nil:
		out.Params.MaxTokens = req.MaxTokens
	case req.MaxCompletionTokens != nil:
		out.Params.MaxTokens = req.MaxCompletionTokens
	}

	if req.ReasoningEffort != nil {
		e := domain.ReasoningEffort(*req.ReasoningEffort)
		if !e.Valid() {
			return nil, &DecodeError{
				Field:   "reasoning_effort",
				Message: fmt.Sprintf("%q is not one of minimal, low, medium, high", *req.ReasoningEffort),
			}
		}
		out.Params.ReasoningEffort = &e
		// Explicitly not from the optimizer: a caller-set effort is a hard
		// capability requirement and must narrow the candidate set.
		out.Params.EffortFromOptimizer = false
	}

	// SessionKey drives prompt-cache affinity. The `user` field is the only
	// stable per-conversation identifier the OpenAI schema offers, and callers
	// already populate it.
	out.SessionKey = req.User

	out.Estimate = domain.Estimate{InputTokens: out.InputTokens()}

	return out, nil
}

// decodeContent handles the string-or-array shape.
func decodeContent(raw json.RawMessage, field string) ([]domain.ContentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, &DecodeError{Field: field + ".content", Message: "is not a valid string"}
		}
		if s == "" {
			return nil, nil
		}
		return []domain.ContentPart{{Kind: domain.PartText, Text: s}}, nil
	}

	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, &DecodeError{
			Field:   field + ".content",
			Message: "must be a string or an array of content parts",
		}
	}

	out := make([]domain.ContentPart, 0, len(parts))
	for i, p := range parts {
		switch p.Type {
		case "text", "input_text", "":
			out = append(out, domain.ContentPart{Kind: domain.PartText, Text: p.Text})
		case "image_url", "input_image":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return nil, &DecodeError{
					Field:   fmt.Sprintf("%s.content[%d].image_url", field, i),
					Message: "is required for an image part",
				}
			}
			out = append(out, decodeImage(p.ImageURL.URL))
		default:
			return nil, &DecodeError{
				Field:   fmt.Sprintf("%s.content[%d].type", field, i),
				Message: fmt.Sprintf("%q is not a supported content type", p.Type),
			}
		}
	}
	return out, nil
}

// decodeImage splits a data: URI into its media type and payload, so adapters
// that require inline base64 do not each re-parse the URI, and adapters that
// require a URL can tell the two forms apart.
func decodeImage(url string) domain.ContentPart {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return domain.ContentPart{Kind: domain.PartImage, URL: url}
	}
	meta, payload, found := strings.Cut(url[len(prefix):], ",")
	if !found {
		return domain.ContentPart{Kind: domain.PartImage, URL: url}
	}
	mediaType, _, _ := strings.Cut(meta, ";")
	return domain.ContentPart{
		Kind:      domain.PartImage,
		MediaType: mediaType,
		Data:      payload,
	}
}

// decodeToolChoice flattens the string-or-object form to a single token.
//
// "auto", "none", "required", or a tool name. Adapters re-expand it into
// whatever shape their provider wants; carrying the raw JSON through would push
// that parsing into three places.
func decodeToolChoice(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Function.Name
	}
	return ""
}
