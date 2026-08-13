// Package ollama adapts Ollama's /api/chat endpoint.
//
// Ollama is in Phase 1 because it is free and local: the whole streaming,
// cancellation, and tool-call path can be exercised end to end without spending
// money or holding a credential. Its wire format is also the simplest of the
// three, which makes it the right place to shake out the adapter contract
// before the awkward vendors arrive.
//
// Notable differences from the others: the stream is newline-delimited JSON
// rather than SSE, tool calls arrive whole rather than in fragments, and
// arguments come as a decoded JSON object rather than a string.
package ollama

import "encoding/json"

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []tool    `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
	Format   any       `json:"format,omitempty"`
	Options  *options  `json:"options,omitempty"`
}

// options is Ollama's name for sampling parameters. Every field is a pointer
// because Ollama applies the model's own default for anything absent, and
// sending a zero would silently override that default with a value the caller
// never chose.
type options struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Seed        *int64   `json:"seed,omitempty"`
	NumPredict  *int     `json:"num_predict,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

func (o *options) empty() bool {
	return o.Temperature == nil && o.TopP == nil && o.Seed == nil &&
		o.NumPredict == nil && len(o.Stop) == 0
}

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Images    []string   `json:"images,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`

	// ToolName pairs a tool result with its call. Ollama models the pairing by
	// name rather than by call ID, so the ID is carried in the request and
	// resolved back on the way out.
	ToolName string `json:"tool_name,omitempty"`
}

type toolCall struct {
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON object, not a string. Every other provider sends a
	// string, so this is the one place the adapter re-encodes rather than
	// passes through.
	Arguments json.RawMessage `json:"arguments,omitempty"`
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

// chatResponse is both the non-streaming body and one line of the stream. The
// same shape serves both, with Done marking the final line.
type chatResponse struct {
	Model      string  `json:"model"`
	CreatedAt  string  `json:"created_at"`
	Message    message `json:"message"`
	Done       bool    `json:"done"`
	DoneReason string  `json:"done_reason,omitempty"`

	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`

	// Error is populated on a 200 response that turns out to be a failure,
	// which Ollama does when a model fails to load mid-stream.
	Error string `json:"error,omitempty"`
}
