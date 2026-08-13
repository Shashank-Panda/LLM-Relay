package openai_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/provider/providertest"
)

func endpoint() *domain.ModelEndpoint {
	return &domain.ModelEndpoint{
		ID:            "openai/gpt-4o-mini@us-east",
		Provider:      openai.ProviderID,
		Model:         "gpt-4o-mini",
		Deployment:    "us-east",
		CredentialRef: "openai-primary",
		Capabilities:  domain.Capabilities{Streaming: true, Tools: true, JSONSchema: true, Vision: true},
		Limits:        domain.Limits{ContextWindow: 128000, MaxOutputTokens: 16384},
	}
}

func TestContract(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Name:     "openai",
		New:      func(c *http.Client) provider.Adapter { return openai.New(c) },
		Endpoint: endpoint(),
		Fixtures: providertest.Fixtures{
			Text:           fixtureText,
			TextStream:     fixtureTextStream,
			ToolCall:       fixtureToolCall,
			ToolCallStream: fixtureToolCallStream,
			UnicodeStream:  fixtureUnicodeStream,
			EmptyStream:    fixtureEmptyStream,
		},
	})
}

const fixtureText = `{
  "id": "chatcmpl-abc",
  "object": "chat.completion",
  "model": "gpt-4o-mini",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "Hello there"},
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
}`

const fixtureTextStream = `data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Hello"}}]}

data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":" there"}}]}

data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`

const fixtureToolCall = `{
  "id": "chatcmpl-abc",
  "model": "gpt-4o-mini",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": null,
      "tool_calls": [{
        "id": "call_abc123",
        "type": "function",
        "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }],
  "usage": {"prompt_tokens": 10, "completion_tokens": 5}
}`

// The arguments are split at offsets that do not align with JSON tokens: after
// an opening brace, mid-key, and mid-string value. Only the concatenation
// parses. An adapter that validates each fragment, or that reorders them,
// produces a tool call the client cannot execute.
//
// Note also that the identity — id, type, name — appears once, on the opening
// delta, and the fragments that follow carry only an index. That is what OpenAI
// actually sends.
const fixtureToolCallStream = `data: {"id":"chatcmpl-abc","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}

data: {"id":"chatcmpl-abc","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\""}}]}}]}

data: {"id":"chatcmpl-abc","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ci"}}]}}]}

data: {"id":"chatcmpl-abc","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Pa"}}]}}]}

data: {"id":"chatcmpl-abc","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ris\"}"}}]}}]}

data: {"id":"chatcmpl-abc","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-abc","model":"gpt-4o-mini","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`

const fixtureUnicodeStream = `data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":"h"}}]}

data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"éllo "}}]}

data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"🌍"}}]}

data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":" caf"}}]}

data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"é"}}]}

data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

const fixtureEmptyStream = `data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"c","model":"gpt-4o-mini","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

func captureRequest(t *testing.T, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, stream bool) (map[string]any, http.Header) {
	t.Helper()

	var (
		got     map[string]any
		headers http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureText))
	}))
	defer srv.Close()

	ep.BaseURL = srv.URL
	a := openai.New(srv.Client())
	cred := provider.Credential{Ref: "openai-primary", APIKey: "sk-test-key"}

	if stream {
		s, err := a.ChatStream(t.Context(), req, ep, cred)
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		s.Close()
	} else if _, err := a.Chat(t.Context(), req, ep, cred); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return got, headers
}

func basicRequest() *domain.NormalizedRequest {
	return &domain.NormalizedRequest{
		Messages: []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "hi"}},
		}},
	}
}

func TestAuthHeader(t *testing.T) {
	_, headers := captureRequest(t, basicRequest(), endpoint(), false)
	if got := headers.Get("Authorization"); got != "Bearer sk-test-key" {
		t.Errorf("Authorization = %q", got)
	}
}

// Without stream_options OpenAI sends no usage block on a streaming response.
// Every streamed request would then record Estimated usage — and streaming is
// most traffic, so cost reporting would lose the majority of it.
func TestStreamRequestsUsage(t *testing.T) {
	got, _ := captureRequest(t, basicRequest(), endpoint(), true)

	opts, _ := got["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage:true", got["stream_options"])
	}
}

func TestNonStreamOmitsStreamOptions(t *testing.T) {
	got, _ := captureRequest(t, basicRequest(), endpoint(), false)
	if _, present := got["stream_options"]; present {
		t.Error("stream_options was sent on a non-streaming request")
	}
}

// A plain text turn uses the string form of content. The array form is legal
// but noisier, and some OpenAI-compatible servers implement only the string
// form — so it is used only where multimodal content requires it.
func TestTextContentUsesTheStringForm(t *testing.T) {
	got, _ := captureRequest(t, basicRequest(), endpoint(), false)

	msgs, _ := got["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if _, isString := first["content"].(string); !isString {
		t.Errorf("content = %#v, want a plain string", first["content"])
	}
}

func TestImageUsesTheArrayForm(t *testing.T) {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{{
			Role: domain.RoleUser,
			Parts: []domain.ContentPart{
				{Kind: domain.PartText, Text: "what is this"},
				{Kind: domain.PartImage, MediaType: "image/png", Data: "iVBOR"},
			},
		}},
	}

	got, _ := captureRequest(t, req, endpoint(), false)
	msgs, _ := got["messages"].([]any)
	first, _ := msgs[0].(map[string]any)

	parts, isArray := first["content"].([]any)
	if !isArray || len(parts) != 2 {
		t.Fatalf("content = %#v, want a two-element array", first["content"])
	}
	img, _ := parts[1].(map[string]any)
	url, _ := img["image_url"].(map[string]any)
	// The data URI is reassembled from the pieces the wire decoder split apart.
	if url["url"] != "data:image/png;base64,iVBOR" {
		t.Errorf("image url = %v", url["url"])
	}
}

// A caller-set effort cannot reach a non-reasoning endpoint — it is a hard
// capability requirement and the filter eliminates it. An optimizer-applied
// default can, and forwarding it to a model that does not understand the
// parameter turns a working request into a 400. Dropping it here is what keeps
// the optimizer's promise that its defaults never change the outcome.
func TestOptimizerEffortIsDroppedForNonReasoningEndpoints(t *testing.T) {
	req := basicRequest()
	req.Params.ReasoningEffort = domain.Ptr(domain.EffortLow)
	req.Params.EffortFromOptimizer = true

	t.Run("dropped where unsupported", func(t *testing.T) {
		got, _ := captureRequest(t, req, endpoint(), false)
		if _, present := got["reasoning_effort"]; present {
			t.Errorf("reasoning_effort sent to an endpoint that does not support it: %v",
				got["reasoning_effort"])
		}
	})

	t.Run("sent where supported", func(t *testing.T) {
		ep := endpoint()
		ep.Capabilities.Reasoning = true
		got, _ := captureRequest(t, req, ep, false)
		if got["reasoning_effort"] != "low" {
			t.Errorf("reasoning_effort = %v, want low", got["reasoning_effort"])
		}
	})
}

func TestToolResultsCarryTheirCallID(t *testing.T) {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{
			{Role: domain.RoleUser, Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "weather?"}}},
			{Role: domain.RoleAssistant, Parts: []domain.ContentPart{
				{Kind: domain.PartToolCall, ToolCallID: "call_1", ToolName: "get_weather", Arguments: `{"city":"Paris"}`},
			}},
			{Role: domain.RoleTool, Parts: []domain.ContentPart{
				{Kind: domain.PartToolResult, ToolCallID: "call_1", Text: "18C"},
			}},
		},
	}

	got, _ := captureRequest(t, req, endpoint(), false)
	msgs, _ := got["messages"].([]any)

	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}

	assistant, _ := msgs[1].(map[string]any)
	// A pure tool-call turn carries content:null — the shape OpenAI emits and
	// expects back.
	if assistant["content"] != nil {
		t.Errorf("assistant content = %#v, want null", assistant["content"])
	}

	result, _ := msgs[2].(map[string]any)
	// OpenAI rejects a tool result it cannot pair with a call.
	if result["tool_call_id"] != "call_1" {
		t.Errorf("tool_call_id = %v, want call_1", result["tool_call_id"])
	}
}

// Status alone is not enough to classify. A context overflow and a malformed
// tool schema are both 400, but one should be rerouted to a larger endpoint and
// the other must not be retried anywhere.
func TestErrorCodeRefinesClassification(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   provider.ErrorClass
	}{
		{
			name:   "context overflow reroutes",
			status: 400,
			body:   `{"error":{"message":"maximum context length","type":"invalid_request_error","code":"context_length_exceeded"}}`,
			want:   provider.ClassReroute,
		},
		{
			name:   "a stale catalog entry reroutes",
			status: 404,
			body:   `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`,
			want:   provider.ClassReroute,
		},
		{
			name:   "a malformed request is terminal",
			status: 400,
			body:   `{"error":{"message":"bad schema","type":"invalid_request_error","code":"invalid_value"}}`,
			want:   provider.ClassTerminal,
		},
		{
			// A rate limit clears on its own; an exhausted quota does not, and
			// retrying it burns the request deadline before failing anyway.
			name:   "exhausted quota is terminal, not a rate limit",
			status: 429,
			body:   `{"error":{"message":"quota exceeded","type":"insufficient_quota","code":"insufficient_quota"}}`,
			want:   provider.ClassTerminal,
		},
		{
			name:   "an ordinary rate limit retries",
			status: 429,
			body:   `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			want:   provider.ClassRetrySame,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			ep := endpoint()
			ep.BaseURL = srv.URL

			_, err := openai.New(srv.Client()).Chat(t.Context(), basicRequest(), ep, provider.Credential{})
			if err == nil {
				t.Fatal("no error")
			}
			if got := provider.ClassOf(err); got != tc.want {
				t.Errorf("class = %s, want %s", got, tc.want)
			}

			// Peeking at the code must not consume the body. The provider's
			// message is what tells the caller how to fix their request, and a
			// classifier that ate it would leave every OpenAI failure reported
			// with an empty message.
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want *provider.Error", err, err)
			}
			if pe.Message == "" {
				t.Error("classification consumed the body and lost the provider's message")
			}
		})
	}
}
