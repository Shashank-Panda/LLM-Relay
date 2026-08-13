// Package anthropic adapts Anthropic's /v1/messages endpoint.
//
// This is the adapter that proves the normalizer is real. Almost nothing about
// the shape matches Relay's own API:
//
//   - the system prompt is a top-level field, not a message
//   - content is always an array of typed blocks, never a bare string
//   - tool calls are tool_use blocks whose input is a JSON object, and tool
//     results are tool_result blocks inside a *user* message
//   - streaming is a state machine of seven event types rather than a sequence
//     of interchangeable deltas
//   - usage arrives in two pieces, at opposite ends of the stream
//   - max_tokens is required, not optional
//
// Every one of those is a place where a pass-through adapter would have been
// silently wrong.
package anthropic

import "encoding/json"

type messagesRequest struct {
	Model  string  `json:"model"`
	System []block `json:"system,omitempty"`

	Messages   []message   `json:"messages"`
	Tools      []tool      `json:"tools,omitempty"`
	ToolChoice *toolChoice `json:"tool_choice,omitempty"`

	// MaxTokens is required by the API. Relay always has a value for it — the
	// caller's, the optimizer's ceiling, or a default — so this is a plain int
	// rather than a pointer.
	MaxTokens int `json:"max_tokens"`

	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`

	Thinking *thinking `json:"thinking,omitempty"`
	Stream   bool      `json:"stream,omitempty"`
}

// thinking is Anthropic's reasoning control. It is a token budget rather than a
// named effort level, so the neutral levels are mapped onto budgets.
type thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type toolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is every content type in one struct.
//
// Anthropic's blocks are a discriminated union and Go has no union type. One
// struct with omitempty on every optional field produces correct JSON for each
// variant, and costs less than four types plus a custom marshaller that would
// have to be kept in sync with them.
type block struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *imageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// CacheControl marks the end of a cacheable prefix. Phase 3 populates it
	// from the optimizer's breakpoints; the field exists now so that placement
	// and encoding stay on opposite sides of the seam from the start.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`

	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// messagesResponse is the non-streaming body.
type messagesResponse struct {
	ID         string  `json:"id"`
	Model      string  `json:"model"`
	Role       string  `json:"role"`
	Content    []block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      usage   `json:"usage"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// streamEvent is any frame of the streaming state machine.
//
// The event types, in order: message_start, then per block
// content_block_start / content_block_delta… / content_block_stop, then
// message_delta carrying the stop reason and output tokens, then message_stop.
// Interleaved with ping events at arbitrary points.
type streamEvent struct {
	Type string `json:"type"`

	// message_start
	Message *messagesResponse `json:"message,omitempty"`

	// content_block_start / _stop
	Index        int    `json:"index"`
	ContentBlock *block `json:"content_block,omitempty"`

	// content_block_delta
	Delta *streamDelta `json:"delta,omitempty"`

	// message_delta
	Usage *usage `json:"usage,omitempty"`

	// error
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type streamDelta struct {
	Type string `json:"type"`

	// text_delta
	Text string `json:"text,omitempty"`

	// input_json_delta — a fragment of the tool's input document, split at
	// arbitrary byte offsets. Only the concatenation parses.
	PartialJSON string `json:"partial_json,omitempty"`

	// thinking_delta
	Thinking string `json:"thinking,omitempty"`

	// message_delta carries the stop reason here rather than at the top level.
	StopReason string `json:"stop_reason,omitempty"`
}

type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
