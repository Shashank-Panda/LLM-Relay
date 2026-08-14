// Package openai adapts OpenAI's /v1/chat/completions endpoint.
//
// This adapter has an obvious shortcut available and does not take it. Relay's
// own API is the OpenAI shape, so bytes could be forwarded almost unchanged.
// That would make every test here pass while proving nothing — the normalizer
// would never be exercised, and its first real workout would be the Anthropic
// adapter, in production, with no coverage. So the request is built from the
// neutral model and the response is decoded back into it, exactly as the other
// adapters do.
//
// The types are declared here rather than reused from internal/wire for the
// same reason: these describe what OpenAI accepts today, and that will drift
// from what Relay accepts.
package openai

import "encoding/json"

type chatRequest struct {
	Model      string    `json:"model"`
	Messages   []message `json:"messages"`
	Tools      []tool    `json:"tools,omitempty"`
	ToolChoice any       `json:"tool_choice,omitempty"`

	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   *int     `json:"max_completion_tokens,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
	Stop        []string `json:"stop,omitempty"`

	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	Stream bool `json:"stream,omitempty"`
	// StreamOptions must be set or a streaming response carries no usage at
	// all. Without it every streamed request would be flagged Estimated and
	// excluded from cost reporting — which is most requests.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`

	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type toolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name string `json:"name,omitempty"`
	// Arguments is a JSON document carried as a string, and in streaming it
	// arrives in fragments split at arbitrary byte offsets.
	Arguments string `json:"arguments,omitempty"`
}

type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict bool            `json:"strict,omitempty"`
}

type chatResponse struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Index        int    `json:"index"`
	Message      *delta `json:"message,omitempty"`
	Delta        *delta `json:"delta,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
}

type delta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`

	// Reasoning is not an OpenAI field. It is accepted because every
	// OpenAI-compatible gateway that exposes thinking content uses this name,
	// and Relay is routinely pointed at those rather than at OpenAI itself.
	Reasoning string `json:"reasoning,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`

	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`

	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// errorBody is the shape OpenAI returns on a failure. The code field is what
// distinguishes a context overflow — which should be rerouted to a bigger
// endpoint — from an ordinary malformed request, which should not be retried
// anywhere.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}
