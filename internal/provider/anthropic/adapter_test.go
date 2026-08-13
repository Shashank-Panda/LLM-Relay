package anthropic_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/anthropic"
	"github.com/Shashank-Panda/relay/internal/provider/providertest"
)

func endpoint() *domain.ModelEndpoint {
	return &domain.ModelEndpoint{
		ID:            "anthropic/claude-sonnet-5@us-east",
		Provider:      anthropic.ProviderID,
		Model:         "claude-sonnet-5",
		Deployment:    "us-east",
		CredentialRef: "anthropic-primary",
		Capabilities: domain.Capabilities{
			Streaming: true, Tools: true, JSONSchema: true, Vision: true, Reasoning: true,
		},
		Limits: domain.Limits{ContextWindow: 200000, MaxOutputTokens: 64000},
	}
}

func TestContract(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Name:     "anthropic",
		New:      func(c *http.Client) provider.Adapter { return anthropic.New(c) },
		Endpoint: endpoint(),
		StatusClasses: map[int]provider.ErrorClass{
			// The contract suite's generic error body has no Anthropic error
			// type, so these fall through to the status mapping. The
			// type-specific behaviour is asserted separately below.
			429: provider.ClassRetrySame,
		},
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
  "id": "msg_abc",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-5",
  "content": [{"type": "text", "text": "Hello there"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 10, "output_tokens": 5}
}`

// Note where the token counts live: input in message_start at the very
// beginning, output in message_delta at the very end. An adapter that reads
// only one of the two reports half the cost of every streamed request.
const fixtureTextStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_abc","model":"claude-sonnet-5","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

const fixtureToolCall = `{
  "id": "msg_abc",
  "model": "claude-sonnet-5",
  "role": "assistant",
  "content": [
    {"type": "tool_use", "id": "toolu_abc123", "name": "get_weather", "input": {"city": "Paris"}}
  ],
  "stop_reason": "tool_use",
  "usage": {"input_tokens": 10, "output_tokens": 5}
}`

// The tool's input arrives as input_json_delta fragments split at offsets that
// do not align with JSON tokens. Note also that content_block_start carries an
// empty input object: treating that as a fragment would prepend "{}" to the
// reassembled document and make it unparseable.
const fixtureToolCallStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_abc","model":"claude-sonnet-5","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_abc123","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ci"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ty\":\"Pa"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

const fixtureUnicodeStream = `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"h"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"éllo "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"🌍"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" caf"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"é"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

const fixtureEmptyStream = `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func captureRequest(t *testing.T, req *domain.NormalizedRequest, ep *domain.ModelEndpoint) (map[string]any, http.Header) {
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
	cred := provider.Credential{Ref: "anthropic-primary", APIKey: "sk-ant-test"}
	if _, err := anthropic.New(srv.Client()).Chat(t.Context(), req, ep, cred); err != nil {
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

// Anthropic authenticates with x-api-key and requires a pinned version header.
// A bearer token is silently a 401.
func TestAuthAndVersionHeaders(t *testing.T) {
	_, h := captureRequest(t, basicRequest(), endpoint())

	if got := h.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q", got)
	}
	if h.Get("anthropic-version") == "" {
		t.Error("anthropic-version is required on every request")
	}
	if h.Get("Authorization") != "" {
		t.Error("a bearer token was sent; Anthropic does not accept one")
	}
}

// The system prompt is a top-level field here, not a message. This is the
// clearest justification for hoisting it during normalization.
func TestSystemIsTopLevel(t *testing.T) {
	req := basicRequest()
	req.System = []domain.ContentPart{{Kind: domain.PartText, Text: "be terse"}}

	got, _ := captureRequest(t, req, endpoint())

	sys, _ := got["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %#v, want a one-element array", got["system"])
	}
	first, _ := sys[0].(map[string]any)
	if first["text"] != "be terse" {
		t.Errorf("system text = %v", first["text"])
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("got %d messages, want only the user turn", len(msgs))
	}
}

// max_tokens is required by the API, so some value is always sent — and it must
// not exceed the model's own limit, which Anthropic rejects outright.
func TestMaxTokensIsAlwaysSentAndClamped(t *testing.T) {
	t.Run("a default is supplied when nobody set one", func(t *testing.T) {
		got, _ := captureRequest(t, basicRequest(), endpoint())
		if got["max_tokens"] == nil {
			t.Fatal("max_tokens missing; Anthropic requires it")
		}
		if got["max_tokens"].(float64) <= 0 {
			t.Errorf("max_tokens = %v", got["max_tokens"])
		}
	})

	t.Run("the caller's value is used", func(t *testing.T) {
		req := basicRequest()
		req.Params.MaxTokens = domain.Ptr(512)
		got, _ := captureRequest(t, req, endpoint())
		if got["max_tokens"] != float64(512) {
			t.Errorf("max_tokens = %v, want 512", got["max_tokens"])
		}
	})

	t.Run("an impossible ceiling is clamped, not rejected", func(t *testing.T) {
		req := basicRequest()
		req.Params.MaxTokens = domain.Ptr(999_999)
		got, _ := captureRequest(t, req, endpoint())
		if got["max_tokens"] != float64(64000) {
			t.Errorf("max_tokens = %v, want the endpoint's 64000 limit", got["max_tokens"])
		}
	})
}

// A tool result is a user-role block here, not a message with a tool role.
// This is the single biggest structural difference from the OpenAI shape, and
// getting it wrong is a 400 on every tool-using turn.
func TestToolResultBecomesAUserBlock(t *testing.T) {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{
			{Role: domain.RoleUser, Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "weather?"}}},
			{Role: domain.RoleAssistant, Parts: []domain.ContentPart{
				{Kind: domain.PartToolCall, ToolCallID: "toolu_1", ToolName: "get_weather", Arguments: `{"city":"Paris"}`},
			}},
			{Role: domain.RoleTool, Parts: []domain.ContentPart{
				{Kind: domain.PartToolResult, ToolCallID: "toolu_1", Text: "18C"},
			}},
		},
	}

	got, _ := captureRequest(t, req, endpoint())
	msgs, _ := got["messages"].([]any)

	if len(msgs) != 3 {
		t.Fatalf("got %d messages: %#v", len(msgs), msgs)
	}

	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	use, _ := blocks[0].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "toolu_1" {
		t.Errorf("assistant block = %#v", use)
	}
	// The input must be an object here, not the string form other providers use.
	if _, isObject := use["input"].(map[string]any); !isObject {
		t.Errorf("input = %#v, want an object", use["input"])
	}

	result, _ := msgs[2].(map[string]any)
	if result["role"] != "user" {
		t.Errorf("tool result role = %v, want user", result["role"])
	}
	rblocks, _ := result["content"].([]any)
	rb, _ := rblocks[0].(map[string]any)
	if rb["type"] != "tool_result" || rb["tool_use_id"] != "toolu_1" {
		t.Errorf("tool result block = %#v", rb)
	}
}

// Anthropic rejects a conversation whose roles do not alternate, so a tool
// result following a user turn has to merge into it rather than open a second
// consecutive user message.
func TestConsecutiveSameRoleMessagesMerge(t *testing.T) {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{
			{Role: domain.RoleUser, Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "a"}}},
			{Role: domain.RoleUser, Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "b"}}},
		},
	}

	got, _ := captureRequest(t, req, endpoint())
	msgs, _ := got["messages"].([]any)

	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want them merged into 1", len(msgs))
	}
	blocks, _ := msgs[0].(map[string]any)
	content, _ := blocks["content"].([]any)
	if len(content) != 2 {
		t.Errorf("merged message has %d blocks, want 2", len(content))
	}
}

// A tool call with no arguments arrives from other providers as an empty
// string. Anthropic requires an object, and sending "" is a 400.
func TestEmptyToolArgumentsBecomeAnObject(t *testing.T) {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{{
			Role: domain.RoleAssistant,
			Parts: []domain.ContentPart{
				{Kind: domain.PartToolCall, ToolCallID: "t1", ToolName: "now", Arguments: ""},
			},
		}},
	}

	got, _ := captureRequest(t, req, endpoint())
	msgs, _ := got["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	blocks, _ := m["content"].([]any)
	b, _ := blocks[0].(map[string]any)

	if _, isObject := b["input"].(map[string]any); !isObject {
		t.Errorf("input = %#v, want {}", b["input"])
	}
}

// Reasoning is a token budget here, not a named level, so the mapping is a
// judgement call rather than a translation. Extended thinking also forbids
// temperature, and sending both is a 400.
func TestThinkingBudget(t *testing.T) {
	req := basicRequest()
	req.Params.ReasoningEffort = domain.Ptr(domain.EffortHigh)
	req.Params.Temperature = domain.Ptr(0.7)
	req.Params.MaxTokens = domain.Ptr(32000)

	got, _ := captureRequest(t, req, endpoint())

	th, _ := got["thinking"].(map[string]any)
	if th == nil {
		t.Fatal("thinking missing for an explicit high effort")
	}
	if th["type"] != "enabled" || th["budget_tokens"].(float64) < 1024 {
		t.Errorf("thinking = %#v", th)
	}
	if _, present := got["temperature"]; present {
		t.Error("temperature was sent alongside extended thinking; Anthropic rejects both together")
	}

	t.Run("minimal effort is honoured as off, not rounded up", func(t *testing.T) {
		// Anthropic's floor is 1024 tokens. Rounding "minimal" up to that would
		// spend a budget the caller asked not to spend.
		req := basicRequest()
		req.Params.ReasoningEffort = domain.Ptr(domain.EffortMinimal)
		got, _ := captureRequest(t, req, endpoint())
		if _, present := got["thinking"]; present {
			t.Errorf("thinking = %v for minimal effort, want absent", got["thinking"])
		}
	})

	t.Run("dropped for an endpoint without reasoning support", func(t *testing.T) {
		ep := endpoint()
		ep.Capabilities.Reasoning = false
		req := basicRequest()
		req.Params.ReasoningEffort = domain.Ptr(domain.EffortHigh)
		got, _ := captureRequest(t, req, ep)
		if _, present := got["thinking"]; present {
			t.Error("thinking sent to an endpoint that does not support it")
		}
	})
}

// Anthropic reports cache reads separately from input tokens, which is the
// opposite of OpenAI. Normalizing to "input includes cached" here is what lets
// Usage.Cost apply one rule across providers.
func TestCachedTokensAreFoldedIntoInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "id":"m","model":"claude-sonnet-5","role":"assistant",
		  "content":[{"type":"text","text":"hi"}],
		  "stop_reason":"end_turn",
		  "usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":900}
		}`))
	}))
	defer srv.Close()

	ep := endpoint()
	ep.BaseURL = srv.URL

	resp, err := anthropic.New(srv.Client()).Chat(t.Context(), basicRequest(), ep, provider.Credential{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if resp.Usage.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000 (100 fresh + 900 cached)", resp.Usage.InputTokens)
	}
	if resp.Usage.CachedInputTokens != 900 {
		t.Errorf("CachedInputTokens = %d, want 900", resp.Usage.CachedInputTokens)
	}
}

func TestErrorTypeRefinesClassification(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   provider.ErrorClass
	}{
		{
			// 529 is outside the range the generic status mapping knows about,
			// and it is explicitly transient.
			name:   "overloaded is retryable",
			status: 529,
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:   provider.ClassRetrySame,
		},
		{
			// A context overflow says the constraint set used for routing was
			// wrong, not that the request is bad. Anthropic states it in the
			// message rather than in a code field.
			name:   "a long prompt reroutes",
			status: 400,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000"}}`,
			want:   provider.ClassReroute,
		},
		{
			name:   "an ordinary bad request is terminal",
			status: 400,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"tools.0.name: invalid"}}`,
			want:   provider.ClassTerminal,
		},
		{
			name:   "auth failure is terminal",
			status: 401,
			body:   `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			want:   provider.ClassTerminal,
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

			_, err := anthropic.New(srv.Client()).Chat(t.Context(), basicRequest(), ep, provider.Credential{})
			if err == nil {
				t.Fatal("no error")
			}

			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want *provider.Error", err, err)
			}
			if pe.Class != tc.want {
				t.Errorf("class = %s, want %s", pe.Class, tc.want)
			}
			// Two peeks happen on some paths. The body must survive both, or
			// the caller loses the message telling them how to fix it.
			if pe.Message == "" {
				t.Error("classification consumed the body and lost the provider's message")
			}
		})
	}
}

// A 200 that turns into an error mid-stream: Anthropic emits an error event
// rather than closing the connection.
func TestErrorEventMidStream(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"c","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	ep := endpoint()
	ep.BaseURL = srv.URL

	st, err := anthropic.New(srv.Client()).ChatStream(t.Context(), basicRequest(), ep, provider.Credential{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer st.Close()

	for {
		_, err := st.Recv()
		if err == nil {
			continue
		}
		if got := provider.ClassOf(err); got != provider.ClassRetrySame {
			t.Errorf("mid-stream error class = %s, want RetrySame", got)
		}
		return
	}
}

// Anthropic's block index counts every content block; the OpenAI shape indexes
// tool calls only. A response with text before a tool call would otherwise emit
// a tool call at index 1 with nothing at index 0 — a gap some clients
// mis-assemble into a phantom first call.
func TestToolIndexIsRelativeToToolCalls(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"c","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"let me check"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	ep := endpoint()
	ep.BaseURL = srv.URL

	st, err := anthropic.New(srv.Client()).ChatStream(t.Context(), basicRequest(), ep, provider.Credential{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer st.Close()

	var (
		text string
		args string
		idx  = -1
	)
	for {
		c, err := st.Recv()
		if err != nil {
			break
		}
		text += c.Text
		if c.ToolCall != nil {
			idx = c.ToolCall.Index
			args += c.ToolCall.Arguments
		}
	}

	if text != "let me check" {
		t.Errorf("text = %q", text)
	}
	if idx != 0 {
		t.Errorf("tool index = %d, want 0 — it counts tool calls, not content blocks", idx)
	}
	if args != `{"city":"Paris"}` {
		t.Errorf("arguments = %q", args)
	}
}
