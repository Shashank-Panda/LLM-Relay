package wire

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
)

func decode(t *testing.T, body string) *domain.NormalizedRequest {
	t.Helper()
	var req ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := Decode(&req)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return got
}

func decodeErr(t *testing.T, body string) *DecodeError {
	t.Helper()
	var req ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, err := Decode(&req)
	var de *DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want *DecodeError", err)
	}
	return de
}

func TestDecodeStringContent(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`)

	if len(r.Messages) != 1 {
		t.Fatalf("got %d messages", len(r.Messages))
	}
	p := r.Messages[0].Parts
	if len(p) != 1 || p[0].Kind != domain.PartText || p[0].Text != "hello" {
		t.Errorf("parts = %+v", p)
	}
}

func TestDecodeArrayContent(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"describe"},
		{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}
	]}]}`)

	p := r.Messages[0].Parts
	if len(p) != 2 {
		t.Fatalf("got %d parts", len(p))
	}
	if p[1].Kind != domain.PartImage || p[1].URL != "https://example.test/a.png" {
		t.Errorf("image part = %+v", p[1])
	}
	if !r.Needs().Vision {
		t.Error("an image in the request did not produce a vision requirement")
	}
}

// TestDecodeDataURI splits the URI once, here, so that adapters requiring
// inline base64 and adapters requiring a URL can tell the forms apart without
// each re-parsing the string.
func TestDecodeDataURI(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
	]}]}`)

	p := r.Messages[0].Parts[0]
	if p.MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", p.MediaType)
	}
	if p.Data != "iVBORw0KGgo=" {
		t.Errorf("Data = %q", p.Data)
	}
	if p.URL != "" {
		t.Errorf("URL = %q, want empty for an inlined image", p.URL)
	}
}

// TestDecodeSystemIsHoisted keeps adapters from re-deriving which messages were
// system ones. Some providers take it as a top-level field, some as a message;
// deciding once here means each adapter renders rather than discovers.
func TestDecodeSystemIsHoisted(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[
		{"role":"system","content":"be terse"},
		{"role":"user","content":"hi"}
	]}`)

	if len(r.System) != 1 || r.System[0].Text != "be terse" {
		t.Errorf("System = %+v", r.System)
	}
	if len(r.Messages) != 1 || r.Messages[0].Role != domain.RoleUser {
		t.Errorf("Messages = %+v", r.Messages)
	}
}

// The developer role is OpenAI's rename of system for reasoning models. Callers
// mix the two freely and both mean the same thing.
func TestDecodeDeveloperRoleIsSystem(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[
		{"role":"developer","content":"be terse"},
		{"role":"user","content":"hi"}
	]}`)
	if len(r.System) != 1 {
		t.Errorf("developer role did not hoist to System: %+v", r.System)
	}
}

func TestDecodeToolCallRoundTrip(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
		]},
		{"role":"tool","tool_call_id":"call_1","content":"18C"}
	]}`)

	if len(r.Messages) != 3 {
		t.Fatalf("got %d messages", len(r.Messages))
	}

	call := r.Messages[1].Parts[0]
	if call.Kind != domain.PartToolCall || call.ToolName != "get_weather" {
		t.Errorf("tool call part = %+v", call)
	}
	if call.Arguments != `{"city":"Paris"}` {
		t.Errorf("Arguments = %q — must survive verbatim, not be re-encoded", call.Arguments)
	}

	result := r.Messages[2].Parts[0]
	if result.Kind != domain.PartToolResult {
		t.Errorf("tool result kind = %s", result.Kind)
	}
	// The pairing is what providers validate. A result whose call ID was
	// dropped is an orphan and every vendor rejects it.
	if result.ToolCallID != "call_1" {
		t.Errorf("ToolCallID = %q, want call_1", result.ToolCallID)
	}
}

// TestDecodePointerParams is the distinction the optimizer depends on. With
// plain ints there is no way to tell temperature:0 from an absent temperature,
// and the optimizer's first rule is that it never overrides a caller's value.
func TestDecodePointerParams(t *testing.T) {
	t.Run("explicit zero is preserved as a decision", func(t *testing.T) {
		r := decode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":0}`)
		if r.Params.Temperature == nil {
			t.Fatal("temperature:0 decoded as unset")
		}
		if *r.Params.Temperature != 0 {
			t.Errorf("Temperature = %v", *r.Params.Temperature)
		}
	})

	t.Run("absent stays nil", func(t *testing.T) {
		r := decode(t, `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
		if r.Params.Temperature != nil || r.Params.MaxTokens != nil || r.Params.Seed != nil {
			t.Error("an absent parameter decoded as set")
		}
	})

	t.Run("max_completion_tokens is the reasoning-model spelling", func(t *testing.T) {
		r := decode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"max_completion_tokens":512}`)
		if r.Params.MaxTokens == nil || *r.Params.MaxTokens != 512 {
			t.Errorf("MaxTokens = %v", r.Params.MaxTokens)
		}
	})
}

// A caller-set effort is a hard capability requirement — send it to a model
// without reasoning support and the provider returns 400 — so it must narrow
// the candidate set. Only an optimizer-applied default must not.
func TestDecodeReasoningEffortIsCallerIntent(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"reasoning_effort":"high"}`)

	if r.Params.ReasoningEffort == nil || *r.Params.ReasoningEffort != domain.EffortHigh {
		t.Fatalf("ReasoningEffort = %v", r.Params.ReasoningEffort)
	}
	if r.Params.EffortFromOptimizer {
		t.Error("a caller-set effort was marked as optimizer-applied")
	}
	if !r.Needs().Reasoning {
		t.Error("a caller-set effort did not become a capability requirement")
	}
}

func TestDecodeStopIsStringOrArray(t *testing.T) {
	one := decode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":"\n\n"}`)
	if len(one.Params.Stop) != 1 || one.Params.Stop[0] != "\n\n" {
		t.Errorf("string form = %q", one.Params.Stop)
	}

	many := decode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":["a","b"]}`)
	if len(many.Params.Stop) != 2 {
		t.Errorf("array form = %q", many.Params.Stop)
	}
}

func TestDecodeToolChoice(t *testing.T) {
	tests := map[string]string{
		`"auto"`:     "auto",
		`"required"`: "required",
		`"none"`:     "none",
		`{"type":"function","function":{"name":"search"}}`: "search",
		`null`: "",
	}
	for raw, want := range tests {
		body := `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":` + raw + `}`
		if got := decode(t, body).ToolChoice; got != want {
			t.Errorf("tool_choice %s = %q, want %q", raw, got, want)
		}
	}
}

func TestDecodeToolsBecomeNeeds(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[
		{"type":"function","function":{"name":"search","description":"find","parameters":{"type":"object"}}}
	]}`)

	if len(r.Tools) != 1 || r.Tools[0].Name != "search" {
		t.Fatalf("Tools = %+v", r.Tools)
	}
	if r.Tools[0].Schema != `{"type":"object"}` {
		t.Errorf("Schema = %q", r.Tools[0].Schema)
	}
	if !r.Needs().Tools {
		t.Error("tools present did not produce a tools requirement")
	}
}

func TestDecodeRejections(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantField string
	}{
		{"no model", `{"messages":[{"role":"user","content":"x"}]}`, "model"},
		{"blank model", `{"model":"  ","messages":[{"role":"user","content":"x"}]}`, "model"},
		{"no messages", `{"model":"m","messages":[]}`, "messages"},
		{"missing role", `{"model":"m","messages":[{"content":"x"}]}`, "messages[0].role"},
		{"unknown role", `{"model":"m","messages":[{"role":"wizard","content":"x"}]}`, "messages[0].role"},
		{"system only", `{"model":"m","messages":[{"role":"system","content":"x"}]}`, "messages"},
		{"unnamed tool", `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{}}]}`, "tools[0].function.name"},
		{"bad effort", `{"model":"m","messages":[{"role":"user","content":"x"}],"reasoning_effort":"maximal"}`, "reasoning_effort"},
		{"unknown content type", `{"model":"m","messages":[{"role":"user","content":[{"type":"video"}]}]}`, "messages[0].content[0].type"},
		{"image with no url", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`, "messages[0].content[0].image_url"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Naming the offending field is the difference between a caller
			// fixing their own request and opening a support ticket.
			if got := decodeErr(t, tc.body).Field; got != tc.wantField {
				t.Errorf("Field = %q, want %q", got, tc.wantField)
			}
		})
	}
}

func TestDecodeNilRequest(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Error("Decode(nil) returned no error")
	}
}

func TestDecodeEstimatesInput(t *testing.T) {
	r := decode(t, `{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("x", 400)+`"}]}`)
	if r.Estimate.InputTokens < 90 {
		t.Errorf("InputTokens = %d, want roughly 100 for 400 bytes", r.Estimate.InputTokens)
	}
}
