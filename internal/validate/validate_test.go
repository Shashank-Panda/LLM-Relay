package validate

import (
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

func text(s string) domain.ContentPart {
	return domain.ContentPart{Kind: domain.PartText, Text: s}
}

func toolCall(name, args string) domain.ContentPart {
	return domain.ContentPart{
		Kind: domain.PartToolCall, ToolCallID: "call_1", ToolName: name, Arguments: args,
	}
}

func resp(parts ...domain.ContentPart) *provider.Response {
	return &provider.Response{Parts: parts, FinishReason: provider.FinishStop}
}

func req() *domain.NormalizedRequest {
	return &domain.NormalizedRequest{
		Messages: []domain.Message{{Role: domain.RoleUser, Parts: []domain.ContentPart{text("hi")}}},
	}
}

func mustInvalid(t *testing.T, got Result, want Reason) {
	t.Helper()
	if got.Valid {
		t.Fatalf("Check = valid, want %s", want)
	}
	if got.Reason != want {
		t.Errorf("reason = %q, want %q", got.Reason, want)
	}
	if got.Detail == "" {
		t.Error("no detail; an escalation nobody can explain is one nobody can act on")
	}
}

func mustValid(t *testing.T, got Result) {
	t.Helper()
	if !got.Valid {
		t.Fatalf("Check = invalid (%s: %s), want valid — a false alarm here buys a "+
			"second provider call for a response that was fine", got.Reason, got.Detail)
	}
}

// --- emptiness ---

func TestEmptyCompletion(t *testing.T) {
	mustInvalid(t, Check(req(), resp()), ReasonEmpty)
	mustInvalid(t, Check(req(), resp(text("   \n\t "))), ReasonEmpty)
}

func TestToolCallIsNotEmpty(t *testing.T) {
	// The obvious wrong implementation: a pure tool-call response carries no
	// text, and treating that as empty would escalate every successful function
	// call a cheap model makes — turning the backstop into a tax on the exact
	// workload it was meant to protect.
	r := req()
	r.Tools = []domain.ToolDef{{Name: "search"}}
	mustValid(t, Check(r, resp(toolCall("search", `{"q":"x"}`))))
}

func TestReasoningIsNotEmpty(t *testing.T) {
	mustValid(t, Check(req(), resp(domain.ContentPart{
		Kind: domain.PartReasoning, Text: "thinking about it",
	})))
}

// --- response format ---

func TestJSONObjectMustParse(t *testing.T) {
	r := req()
	r.ResponseFormat = domain.ResponseFormat{Type: domain.FormatJSONObject}

	mustValid(t, Check(r, resp(text(`{"ok":true}`))))
	mustInvalid(t, Check(r, resp(text("Sure! Here is your JSON: {ok: true}"))), ReasonInvalidJSON)
}

func TestPlainTextIsNeverJSONChecked(t *testing.T) {
	// No response_format means the caller asked for prose, and prose is not
	// invalid for failing to be JSON.
	mustValid(t, Check(req(), resp(text("not json at all"))))
}

// --- schema subset ---

func schemaReq(schema string) *domain.NormalizedRequest {
	r := req()
	r.ResponseFormat = domain.ResponseFormat{Type: domain.FormatJSONSchema, Schema: schema}
	return r
}

func TestSchemaViolations(t *testing.T) {
	const schema = `{
		"type": "object",
		"properties": {
			"sentiment": {"type": "string", "enum": ["positive", "negative"]},
			"score": {"type": "number"},
			"tags": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["sentiment", "score"],
		"additionalProperties": false
	}`

	tests := map[string]struct {
		body string
		want Reason
	}{
		"conforming":            {`{"sentiment":"positive","score":0.9}`, ReasonValid},
		"conforming with array": {`{"sentiment":"negative","score":0.1,"tags":["a","b"]}`, ReasonValid},
		"missing required":      {`{"sentiment":"positive"}`, ReasonSchemaViolation},
		"wrong type":            {`{"sentiment":"positive","score":"high"}`, ReasonSchemaViolation},
		"outside enum":          {`{"sentiment":"ecstatic","score":0.9}`, ReasonSchemaViolation},
		"extra property":        {`{"sentiment":"positive","score":0.9,"extra":1}`, ReasonSchemaViolation},
		"wrong item type":       {`{"sentiment":"positive","score":0.9,"tags":[1,2]}`, ReasonSchemaViolation},
		"not an object":         {`["positive"]`, ReasonSchemaViolation},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := Check(schemaReq(schema), resp(text(tc.body)))
			if tc.want == ReasonValid {
				mustValid(t, got)
				return
			}
			mustInvalid(t, got, tc.want)
		})
	}
}

func TestIntegerAcceptsAWholeNumber(t *testing.T) {
	// JSON has no integer type and encoding/json decodes every number to
	// float64, so 3 arrives indistinguishable from 3.0. A checker that rejected
	// it would flag correct output on every schema that uses integers — one of
	// the two or three ways a naive validator becomes a bill.
	const schema = `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`
	mustValid(t, Check(schemaReq(schema), resp(text(`{"n": 3}`))))
	mustInvalid(t, Check(schemaReq(schema), resp(text(`{"n": 3.5}`))), ReasonSchemaViolation)
}

func TestUnsupportedSchemaConstructsAreSkipped(t *testing.T) {
	// The subset errs toward valid on purpose. anyOf means the document need
	// only satisfy one branch, and a checker that tested the wrong branch would
	// report a violation that is not one — spending a provider call to be wrong.
	for name, schema := range map[string]string{
		"anyOf": `{"anyOf":[{"type":"string"},{"type":"object"}]}`,
		"oneOf": `{"oneOf":[{"type":"number"}]}`,
		"allOf": `{"allOf":[{"type":"string"}]}`,
		"$ref":  `{"$ref":"#/definitions/thing"}`,
	} {
		t.Run(name, func(t *testing.T) {
			mustValid(t, Check(schemaReq(schema), resp(text(`{"anything":true}`))))
		})
	}
}

func TestUnparseableSchemaIsNotTheModelsFault(t *testing.T) {
	// The caller supplied a broken schema. Escalating to a dearer model would
	// produce the same output against the same broken schema, at twice the
	// price and with the same outcome.
	mustValid(t, Check(schemaReq(`{"type": oops}`), resp(text(`{"a":1}`))))
}

// --- tool calls ---

func TestToolCallArgumentsMustParse(t *testing.T) {
	r := req()
	r.Tools = []domain.ToolDef{{Name: "search"}}

	mustValid(t, Check(r, resp(toolCall("search", `{"q":"x"}`))))
	mustInvalid(t, Check(r, resp(toolCall("search", `{"q": }`))), ReasonInvalidToolCall)
}

func TestUndeclaredTool(t *testing.T) {
	r := req()
	r.Tools = []domain.ToolDef{{Name: "search"}}
	mustInvalid(t, Check(r, resp(toolCall("rm_rf", `{}`))), ReasonUndeclaredTool)
}

func TestZeroArgumentToolCall(t *testing.T) {
	// Empty arguments are how a tool with no parameters is called. Rejecting
	// them would escalate a correct call to a correct tool.
	r := req()
	r.Tools = []domain.ToolDef{{Name: "ping"}}
	mustValid(t, Check(r, resp(toolCall("ping", ""))))
}

func TestToolCallWithNoDeclaredToolsIsNotJudged(t *testing.T) {
	// A provider returning a tool call for a request that offered no tools is
	// odd, but the request declared no vocabulary to check it against, and
	// inventing one would be guessing.
	mustValid(t, Check(req(), resp(toolCall("something", `{}`))))
}

// --- refusal ---

func TestContentFilterIsARefusal(t *testing.T) {
	r := &provider.Response{
		Parts:        []domain.ContentPart{text("I can't help with that.")},
		FinishReason: provider.FinishContentFilter,
	}
	mustInvalid(t, Check(req(), r), ReasonRefusal)
}

func TestRefusalPhrasingAloneIsNotARefusal(t *testing.T) {
	// The deliberately absent heuristic. Text-matching refusal phrasing is
	// locale-specific, defeated by paraphrase, and fires on the perfectly good
	// answer to "what should I say when I have to decline a request" — which is
	// exactly this test. A false alarm buys a second call for a correct answer,
	// so a heuristic with a real false-positive rate is worse than none.
	mustValid(t, Check(req(), resp(text(
		`A polite decline usually starts with "I'm sorry, I can't help with that."`))))
}

// --- shape ---

func TestNilsAreValid(t *testing.T) {
	mustValid(t, Check(nil, nil))
	mustValid(t, Check(req(), nil))
	mustValid(t, Check(nil, resp(text("hi"))))
}

func TestOrdinaryResponsesPass(t *testing.T) {
	// The case that runs on essentially every request. If this ever fails,
	// every downgraded request escalates and the product costs double.
	r := req()
	r.Tools = []domain.ToolDef{{Name: "search"}}
	for name, response := range map[string]*provider.Response{
		"prose":         resp(text("The capital of France is Paris.")),
		"code":          resp(text("```go\nfunc main() {}\n```")),
		"tool call":     resp(toolCall("search", `{"q":"paris"}`)),
		"text and tool": resp(text("Let me look."), toolCall("search", `{"q":"paris"}`)),
	} {
		t.Run(name, func(t *testing.T) { mustValid(t, Check(r, response)) })
	}
}
