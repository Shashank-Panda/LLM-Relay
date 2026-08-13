package wire

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestEncodeResponseContentIsNullForToolCalls guards a distinction SDKs branch
// on. A pure tool-call response carries content:null; emitting "" instead makes
// a tool call look like an empty answer, and clients that check for falsy
// content treat it as a refusal.
func TestEncodeResponseContentIsNullForToolCalls(t *testing.T) {
	resp := &provider.Response{
		ID: "x",
		Parts: []domain.ContentPart{{
			Kind: domain.PartToolCall, ToolCallID: "call_1",
			ToolName: "search", Arguments: `{"q":"go"}`,
		}},
		FinishReason: provider.FinishToolCalls,
	}

	got := marshal(t, EncodeResponse(resp, "relay/fast-coder", 1))

	if !strings.Contains(got, `"content":null`) {
		t.Errorf("want content:null in %s", got)
	}
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("want finish_reason tool_calls in %s", got)
	}
	if !strings.Contains(got, `"arguments":"{\"q\":\"go\"}"`) {
		t.Errorf("arguments were not carried verbatim: %s", got)
	}
}

func TestEncodeResponseEmptyTextIsNotNull(t *testing.T) {
	// An empty answer with no tool calls is content:"" — a real, if useless,
	// completion. Only the tool-call case is null.
	resp := &provider.Response{ID: "x", FinishReason: provider.FinishStop}
	got := marshal(t, EncodeResponse(resp, "m", 1))
	if !strings.Contains(got, `"content":""`) {
		t.Errorf("want content:\"\" in %s", got)
	}
}

// The model field echoes what the caller asked for, not what served it.
// Clients key caches and dashboards on this; substituting the served model
// would break them silently. Substitution is disclosed in headers instead.
func TestEncodeResponseEchoesRequestedModel(t *testing.T) {
	resp := &provider.Response{ID: "x", Model: "anthropic/claude-sonnet-5@us-east"}
	got := EncodeResponse(resp, "claude-opus-5", 1)
	if got.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want the requested name", got.Model)
	}
}

func TestEncodeResponseSeparatesReasoning(t *testing.T) {
	resp := &provider.Response{
		ID: "x",
		Parts: []domain.ContentPart{
			{Kind: domain.PartReasoning, Text: "thinking"},
			{Kind: domain.PartText, Text: "answer"},
		},
	}
	out := EncodeResponse(resp, "m", 1)
	msg := out.Choices[0].Message
	if msg.Content == nil || *msg.Content != "answer" {
		t.Errorf("Content = %v, want just the answer", msg.Content)
	}
	if msg.Reasoning != "thinking" {
		t.Errorf("Reasoning = %q", msg.Reasoning)
	}
}

// TestEncodeChunkToolCallIdentityOnce mirrors what providers actually emit: the
// id, type, and name appear on the opening delta for an index and never again.
// Repeating them on every fragment is a shape some SDKs mis-assemble into
// duplicated calls.
func TestEncodeChunkToolCallIdentityOnce(t *testing.T) {
	open := EncodeChunk(&provider.Chunk{
		ToolCall: &provider.ToolCallDelta{Index: 0, ID: "call_1", Name: "search", Arguments: `{"q`},
	}, "id", "m", 1)

	tc := open.Choices[0].Delta.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "search" || tc.Type != "function" {
		t.Errorf("opening delta = %+v", tc)
	}
	if tc.Index == nil || *tc.Index != 0 {
		t.Errorf("Index = %v, want 0", tc.Index)
	}

	cont := EncodeChunk(&provider.Chunk{
		ToolCall: &provider.ToolCallDelta{Index: 0, Arguments: `":"go"}`},
	}, "id", "m", 1)

	tc = cont.Choices[0].Delta.ToolCalls[0]
	if tc.ID != "" || tc.Function.Name != "" || tc.Type != "" {
		t.Errorf("continuation delta repeated identity: %+v", tc)
	}
	if tc.Function.Arguments != `":"go"}` {
		t.Errorf("Arguments = %q", tc.Function.Arguments)
	}
}

// A frame carrying no text renders as "delta":{}, not "delta":{"content":null}.
// That is what OpenAI emits, and matching the bytes exactly is worth a separate
// type: SDKs are written against observed output, and "probably tolerated" is a
// weaker guarantee than "identical".
//
// The non-streaming case is the opposite and must stay so — see
// TestEncodeResponseContentIsNullForToolCalls.
func TestEmptyDeltaOmitsContent(t *testing.T) {
	finish := marshal(t, EncodeChunk(&provider.Chunk{FinishReason: provider.FinishStop}, "id", "m", 1))
	if strings.Contains(finish, `"content"`) {
		t.Errorf("finish frame carries a content field: %s", finish)
	}
	if !strings.Contains(finish, `"delta":{}`) {
		t.Errorf("want delta:{} in %s", finish)
	}

	// An empty string is still a value the provider sent, and survives.
	empty := marshal(t, EncodeChunk(&provider.Chunk{Text: ""}, "id", "m", 1))
	if strings.Contains(empty, `"content":null`) {
		t.Errorf("want no null content in %s", empty)
	}
}

func TestRoleChunkOpensTheStream(t *testing.T) {
	// SDKs that build a message incrementally use this frame to initialise the
	// object. Without it the first content delta is applied to nothing.
	got := marshal(t, RoleChunk("id", "m", 1))
	if !strings.Contains(got, `"role":"assistant"`) {
		t.Errorf("want role:assistant in %s", got)
	}
	if !strings.Contains(got, `"object":"chat.completion.chunk"`) {
		t.Errorf("want chunk object in %s", got)
	}
}

// The usage frame carries an empty choices array, not an absent one. That is
// what OpenAI emits and therefore what SDKs parse without complaint.
func TestUsageChunkHasEmptyChoicesArray(t *testing.T) {
	got := marshal(t, UsageChunk(provider.Usage{InputTokens: 10, OutputTokens: 5}, "id", "m", 1))
	if !strings.Contains(got, `"choices":[]`) {
		t.Errorf("want choices:[] in %s", got)
	}
	if !strings.Contains(got, `"total_tokens":15`) {
		t.Errorf("want total_tokens:15 in %s", got)
	}
}

func TestEncodeUsageDetails(t *testing.T) {
	u := provider.Usage{InputTokens: 100, CachedInputTokens: 80, OutputTokens: 20, ReasoningTokens: 5}
	got := marshal(t, encodeUsage(u))
	for _, want := range []string{`"cached_tokens":80`, `"reasoning_tokens":5`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in %s", want, got)
		}
	}

	if encodeUsage(provider.Usage{}) != nil {
		t.Error("empty usage should be omitted, not reported as zeros")
	}
}

// A caller may legitimately put an endpoint ID, an alias, or a route name in
// the model field. Listing only endpoints would make Relay's own virtual models
// look unavailable to anything that checks the list first.
func TestEncodeModelsIncludesAliasesAndRoutes(t *testing.T) {
	cat := &domain.Catalog{
		Endpoints: map[string]*domain.ModelEndpoint{
			"anthropic/sonnet@us": {ID: "anthropic/sonnet@us", Provider: "anthropic"},
			"old/model@us":        {ID: "old/model@us", Provider: "old", Lifecycle: domain.Lifecycle{Status: domain.StatusRetired}},
		},
		Aliases: map[string]string{"claude-sonnet-5": "anthropic/sonnet@us"},
		Routes:  map[string]*domain.Route{"relay/fast-coder": {Name: "relay/fast-coder"}},
	}

	got := EncodeModels(cat, 1)

	ids := make(map[string]string, len(got.Data))
	for _, m := range got.Data {
		ids[m.ID] = m.OwnedBy
	}

	for _, want := range []string{"anthropic/sonnet@us", "claude-sonnet-5", "relay/fast-coder"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("%q missing from the model list", want)
		}
	}
	// A retired endpoint is not callable, so advertising it invites a request
	// that is guaranteed to be rejected during filtering.
	if _, ok := ids["old/model@us"]; ok {
		t.Error("a retired endpoint was advertised")
	}
	if ids["claude-sonnet-5"] != "anthropic" {
		t.Errorf("alias owned_by = %q, want the target's provider", ids["claude-sonnet-5"])
	}
}

func TestEncodeModelsNilCatalog(t *testing.T) {
	got := EncodeModels(nil, 1)
	if got.Object != "list" || got.Data == nil {
		t.Errorf("want an empty list, got %+v", got)
	}
	// Data must marshal as [] rather than null: SDKs iterate it without a check.
	if s := marshal(t, got); !strings.Contains(s, `"data":[]`) {
		t.Errorf("want data:[] in %s", s)
	}
}

func TestStreamWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewStreamWriter(rec)

	if s.Started() {
		t.Error("Started() before any frame — the status code must stay open so a " +
			"pre-first-byte failure can still produce a JSON error")
	}

	if err := s.Send(map[string]string{"a": "1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !s.Started() {
		t.Error("Started() false after a frame")
	}
	_ = s.Comment("keep-alive")
	_ = s.Done()

	body := rec.Body.String()
	want := "data: {\"a\":\"1\"}\n\n: keep-alive\n\ndata: [DONE]\n\n"
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}

	h := rec.Header()
	for k, v := range map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
		// Without this, nginx and most CDNs buffer the whole response and
		// deliver it at the end — streaming that silently is not streaming.
		"X-Accel-Buffering": "no",
	} {
		if h.Get(k) != v {
			t.Errorf("header %s = %q, want %q", k, h.Get(k), v)
		}
	}
}

// After the first frame the status code is fixed, so an error can only be
// delivered inside the stream. Closing the connection instead is
// indistinguishable at the client from a network fault, and a client that
// cannot tell those apart will retry a request guaranteed to fail again.
func TestStreamWriterErrorAfterStart(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewStreamWriter(rec)

	_ = s.Send(map[string]string{"a": "1"})
	s.Error("upstream exploded", TypeAPIError, "RetrySame")

	body := rec.Body.String()
	if !strings.Contains(body, `"message":"upstream exploded"`) {
		t.Errorf("error frame missing from %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream did not terminate with [DONE]: %q", body)
	}
}
