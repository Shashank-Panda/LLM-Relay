package ollama_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/ollama"
	"github.com/Shashank-Panda/relay/internal/provider/providertest"
)

func endpoint() *domain.ModelEndpoint {
	return &domain.ModelEndpoint{
		ID:            "ollama/qwen-coder@local",
		Provider:      ollama.ProviderID,
		Model:         "qwen2.5-coder",
		Deployment:    "local",
		CredentialRef: "local",
		Capabilities:  domain.Capabilities{Streaming: true, Tools: true},
		Limits:        domain.Limits{ContextWindow: 32768, MaxOutputTokens: 8192},
	}
}

func TestContract(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Name:              "ollama",
		New:               func(c *http.Client) provider.Adapter { return ollama.New(c) },
		Endpoint:          endpoint(),
		StreamContentType: "application/x-ndjson",
		StatusClasses: map[int]provider.ErrorClass{
			// A 404 from Ollama means the model is not pulled on this host, not
			// that the URL is wrong. Another endpoint can serve the request, so
			// failing the caller would be giving up prematurely.
			404: provider.ClassReroute,
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
  "model": "qwen2.5-coder",
  "created_at": "2026-08-12T10:00:00Z",
  "message": {"role": "assistant", "content": "Hello there"},
  "done": true,
  "done_reason": "stop",
  "prompt_eval_count": 10,
  "eval_count": 5
}`

// Ollama streams newline-delimited JSON, not SSE. Each line is a whole message.
const fixtureTextStream = `{"model":"qwen2.5-coder","message":{"role":"assistant","content":"Hello"},"done":false}
{"model":"qwen2.5-coder","message":{"role":"assistant","content":" there"},"done":false}
{"model":"qwen2.5-coder","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":5}
`

// Ollama sends tool arguments as a decoded object rather than a string, which
// is the one place this adapter re-encodes rather than passing bytes through.
const fixtureToolCall = `{
  "model": "qwen2.5-coder",
  "message": {
    "role": "assistant",
    "content": "",
    "tool_calls": [{"function": {"name": "get_weather", "arguments": {"city": "Paris"}}}]
  },
  "done": true,
  "done_reason": "stop",
  "prompt_eval_count": 10,
  "eval_count": 5
}`

// Ollama emits a tool call complete in one message rather than in fragments, so
// this is the degenerate case of the reassembly contract: one fragment. The
// suite still asserts it reassembles, because "one" must not be special-cased
// anywhere downstream.
const fixtureToolCallStream = `{"model":"qwen2.5-coder","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done":false}
{"model":"qwen2.5-coder","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":5}
`

// The é, the globe, and the é of café each land on a frame boundary. Because
// these are JSON string values the escaping protects the runes, but a reader
// that slices raw bytes rather than parsing frames still corrupts them.
const fixtureUnicodeStream = `{"model":"qwen2.5-coder","message":{"role":"assistant","content":"héllo "},"done":false}
{"model":"qwen2.5-coder","message":{"role":"assistant","content":"🌍"},"done":false}
{"model":"qwen2.5-coder","message":{"role":"assistant","content":" café"},"done":false}
{"model":"qwen2.5-coder","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":5}
`

const fixtureEmptyStream = `{"model":"qwen2.5-coder","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":5}
`

// captureRequest runs one call against a server that records what was sent.
func captureRequest(t *testing.T, req *domain.NormalizedRequest, stream bool) map[string]any {
	t.Helper()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureText))
	}))
	defer srv.Close()

	ep := endpoint()
	ep.BaseURL = srv.URL
	a := ollama.New(srv.Client())

	if stream {
		s, err := a.ChatStream(t.Context(), req, ep, provider.Credential{})
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		s.Close()
	} else if _, err := a.Chat(t.Context(), req, ep, provider.Credential{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return got
}

func TestRequestEncoding(t *testing.T) {
	req := &domain.NormalizedRequest{
		System: []domain.ContentPart{{Kind: domain.PartText, Text: "be terse"}},
		Messages: []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "hi"}},
		}},
		Tools: []domain.ToolDef{{
			Name: "search", Description: "find things", Schema: `{"type":"object"}`,
		}},
		Params: domain.SamplingParams{
			Temperature: domain.Ptr(0.0),
			MaxTokens:   domain.Ptr(256),
			Stop:        []string{"\n\n"},
		},
	}

	got := captureRequest(t, req, false)

	if got["model"] != "qwen2.5-coder" {
		t.Errorf("model = %v, want the endpoint's provider-side name", got["model"])
	}
	if got["stream"] != false {
		t.Errorf("stream = %v, want false", got["stream"])
	}

	// Ollama takes the system prompt as a message, so the hoisting the wire
	// decoder did must be undone here rather than in every adapter.
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want system + user", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be terse" {
		t.Errorf("first message = %+v, want the system prompt", first)
	}

	opts, _ := got["options"].(map[string]any)
	if opts == nil {
		t.Fatal("options missing")
	}
	// temperature:0 is a caller demanding determinism, and it must survive as a
	// value rather than being dropped as a zero.
	if opts["temperature"] != float64(0) {
		t.Errorf("temperature = %v, want an explicit 0", opts["temperature"])
	}
	if opts["num_predict"] != float64(256) {
		t.Errorf("num_predict = %v, want 256", opts["num_predict"])
	}
}

// An absent parameter must not become a zero on the wire. Ollama applies the
// model's own default for anything omitted; sending 0 would silently override a
// default the caller never intended to touch.
func TestUnsetParamsAreOmitted(t *testing.T) {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "hi"}},
		}},
	}

	got := captureRequest(t, req, false)

	if _, present := got["options"]; present {
		t.Errorf("options was sent although the caller set nothing: %v", got["options"])
	}
}

func TestStreamFlagIsSet(t *testing.T) {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "hi"}},
		}},
	}
	if got := captureRequest(t, req, true); got["stream"] != true {
		t.Errorf("stream = %v on a streaming call, want true", got["stream"])
	}
}

// A tool result carries the call it answers. Ollama pairs by name, and one
// assistant turn holding several results has to become several messages —
// flattening them would lose the pairing, and a result the model cannot match
// to a call is one it cannot use.
func TestToolResultsBecomeSeparateMessages(t *testing.T) {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{
			{Role: domain.RoleUser, Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "weather?"}}},
			{Role: domain.RoleTool, Parts: []domain.ContentPart{
				{Kind: domain.PartToolResult, ToolCallID: "call_0", ToolName: "get_weather", Text: "18C"},
				{Kind: domain.PartToolResult, ToolCallID: "call_1", ToolName: "get_time", Text: "10:00"},
			}},
		},
	}

	got := captureRequest(t, req, false)
	msgs, _ := got["messages"].([]any)

	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want user + two tool results", len(msgs))
	}
	for i, want := range []string{"get_weather", "get_time"} {
		m, _ := msgs[i+1].(map[string]any)
		if m["tool_name"] != want {
			t.Errorf("message %d tool_name = %v, want %q", i+1, m["tool_name"], want)
		}
	}
}

// A 200 carrying an error field is a failure, not an empty answer. Ollama does
// this when a model fails to load; reporting it as success would bill the
// caller for a blank completion.
func TestErrorInsideA200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"model requires more system memory"}`))
	}))
	defer srv.Close()

	ep := endpoint()
	ep.BaseURL = srv.URL

	_, err := ollama.New(srv.Client()).Chat(t.Context(),
		&domain.NormalizedRequest{Messages: []domain.Message{{Role: domain.RoleUser}}},
		ep, provider.Credential{})

	if err == nil {
		t.Fatal("a 200 carrying an error was reported as success")
	}
	if got := provider.ClassOf(err); got != provider.ClassRetrySame {
		t.Errorf("class = %s, want RetrySame", got)
	}
}

// Missing counts are flagged rather than recorded as a measured zero. An
// approximate number indistinguishable from a measured one corrupts every
// aggregate it enters.
func TestUsageEstimatedWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"hi"},"done":true}`))
	}))
	defer srv.Close()

	ep := endpoint()
	ep.BaseURL = srv.URL

	resp, err := ollama.New(srv.Client()).Chat(t.Context(),
		&domain.NormalizedRequest{Messages: []domain.Message{{Role: domain.RoleUser}}},
		ep, provider.Credential{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !resp.Usage.Estimated {
		t.Error("absent usage was not flagged as estimated")
	}
}
