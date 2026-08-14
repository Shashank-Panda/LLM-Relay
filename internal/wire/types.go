// Package wire is Relay's public HTTP API: the OpenAI chat-completions shape.
//
// This is deliberately a separate package from internal/provider/openai, which
// is an *outbound* adapter. They look almost identical today and will not stay
// that way — Relay adds disclosure headers and decision metadata, and OpenAI
// adds fields Relay does not implement. Collapsing them is the shortcut that
// turns the normalizer into a pass-through that only appears to work, and the
// bill for it arrives with the second provider.
//
// Compatibility is the entire integration story: a customer changes a base URL
// and nothing else. Every field here exists because a real SDK sends it.
package wire

import "encoding/json"

// ChatRequest is POST /v1/chat/completions.
type ChatRequest struct {
	Model      string          `json:"model"`
	Messages   []Message       `json:"messages"`
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`

	// Pointers throughout, because unset and zero are different requests.
	//
	// temperature:0 is a caller demanding determinism; an absent temperature is
	// a caller with no opinion. max_tokens:0 is nonsense but an absent
	// max_tokens is the case the optimizer's output ceiling exists to fill. A
	// plain int cannot tell these apart, and the optimizer's first rule is that
	// it never overrides an explicit caller value.
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	MaxTokens           *int     `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
	Seed                *int64   `json:"seed,omitempty"`
	Stop                Stop     `json:"stop,omitempty"`
	ReasoningEffort     *string  `json:"reasoning_effort,omitempty"`

	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// User and Metadata are carried through for the customer's own analytics.
	User     string            `json:"user,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Message is one turn. Content is string-or-array, which is the single most
// annoying piece of this format and is not optional to support: every SDK sends
// a bare string for text, and the array form for anything multimodal.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// ContentPart is one element of the array form of content.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
	// Detail is accepted and ignored: it is a provider-side resolution hint
	// with no neutral meaning. Rejecting it would break real callers.
	Detail string `json:"detail,omitempty"`
}

type ToolCall struct {
	// Index is present only on streaming deltas, where it identifies which of
	// several concurrent calls a fragment belongs to.
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name,omitempty"`
	// Arguments is a JSON document carried as a string. In streaming it arrives
	// as fragments that concatenate into one.
	Arguments string `json:"arguments,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ResponseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

type JSONSchema struct {
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
}

// ChatResponse is a non-streaming completion.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`

	// SystemFingerprint is echoed for SDK compatibility. Relay has no
	// meaningful value for it and omits it rather than inventing one.
	SystemFingerprint string `json:"system_fingerprint,omitempty"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      *ChoiceMessage  `json:"message,omitempty"`
	Delta        *Delta          `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

// ChoiceMessage is a complete assistant message, on a non-streaming response.
//
// Content has no omitempty, deliberately. A pure tool-call response must carry
// content:null — SDKs branch on exactly that, and emitting "" instead makes a
// tool call look like an empty answer to anything checking for falsy content.
type ChoiceMessage struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// Reasoning carries thinking-block content. Not an OpenAI field; emitted
	// only when the served endpoint produced one, under the name the ecosystem
	// has converged on.
	Reasoning string `json:"reasoning,omitempty"`
}

// Delta is one increment of a streaming message.
//
// Separate from ChoiceMessage for one field's sake: here Content *is* omitempty,
// so a frame carrying no text renders as "delta":{} rather than
// "delta":{"content":null}. That is what OpenAI emits, and matching it exactly
// is worth a type — SDKs are written against observed bytes, and "probably
// tolerated" is a weaker guarantee than "identical".
type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Reasoning string     `json:"reasoning,omitempty"`
}

// ChatChunk is one SSE frame of a streaming completion.
type ChatChunk struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`

	// Usage appears on a final chunk with an empty Choices array, which is what
	// OpenAI does and therefore what SDKs expect.
	Usage *Usage `json:"usage,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Model is one entry of GET /v1/models.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
