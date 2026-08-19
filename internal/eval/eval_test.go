package eval

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// --- suite loading ---

func TestLoadRejections(t *testing.T) {
	tests := map[string]struct {
		yaml string
		want string
	}{
		"no id":        {"cases:\n  - dimension: coding\n    prompt: hi\n", "id is required"},
		"no dimension": {"cases:\n  - id: a\n    prompt: hi\n", "dimension is required"},
		"no prompt":    {"cases:\n  - id: a\n    dimension: coding\n", "prompt is required"},
		"duplicate id": {
			"cases:\n  - id: a\n    dimension: c\n    prompt: x\n" +
				"  - id: a\n    dimension: c\n    prompt: y\n", "duplicate id"},
		"tool expectation with no tools": {
			"cases:\n  - id: a\n    dimension: c\n    prompt: x\n    expect:\n      calls_tool: search\n",
			"offers no tools"},
		"bad response format": {
			"cases:\n  - id: a\n    dimension: c\n    prompt: x\n    response_format: xml\n",
			"response_format must be"},
		"schema format without a schema": {
			"cases:\n  - id: a\n    dimension: c\n    prompt: x\n    response_format: json_schema\n",
			"needs a schema"},
		// Strict decoding, like every other config file here. A misspelled
		// expectation that is silently ignored produces a case that passes
		// unconditionally, and a quality score built partly from unconditional
		// passes is worse than no score at all.
		"misspelled field": {
			"cases:\n  - id: a\n    dimension: c\n    prompt: x\n    expect:\n      contain: [hi]\n",
			"field contain not found"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tc.yaml))
			if err == nil {
				t.Fatalf("loaded a suite with %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	s, err := Load(strings.NewReader("cases:\n  - id: a\n    dimension: coding\n    prompt: hi\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Repeats != 1 {
		t.Errorf("Repeats = %d, want 1", s.Repeats)
	}
	if s.Cases[0].Weight != 1 {
		t.Errorf("Weight = %v, want 1", s.Cases[0].Weight)
	}
	// A case with no expectations still measures something real: that the
	// response was well-formed.
	if !s.Cases[0].Expect.ValidOnly {
		t.Error("a case with no expectations did not fall back to a validity check")
	}
}

func TestShippedSuiteLoads(t *testing.T) {
	// The suite in config/ is documentation as much as configuration, and a
	// documented example that does not parse is worse than none.
	s, err := LoadFile("../../config/eval/coding.yaml")
	if err != nil {
		t.Fatalf("loading the shipped suite: %v", err)
	}
	if len(s.Cases) == 0 {
		t.Fatal("the shipped suite has no cases")
	}
	dims := map[string]bool{}
	for _, c := range s.Cases {
		dims[c.Dimension] = true
	}
	if len(dims) < 2 {
		t.Errorf("the shipped suite scores %d dimension(s); a single quality number "+
			"is a poor predictor for any specific workload", len(dims))
	}
}

// --- grading ---

func caseWith(expect Expect) Case {
	return Case{ID: "c", Dimension: "coding", Weight: 1, Prompt: "hi", Expect: expect}
}

func textResp(s string) *provider.Response {
	return &provider.Response{
		Parts:        []domain.ContentPart{{Kind: domain.PartText, Text: s}},
		FinishReason: provider.FinishStop,
	}
}

func TestGradeContains(t *testing.T) {
	c := caseWith(Expect{Contains: []string{"Paris"}})
	req := c.Request(0)

	if g := grade(c, req, textResp("The capital is Paris.")); !g.Passed {
		t.Errorf("failed a passing case: %v", g.Failures)
	}
	// Case-insensitive, because an eval grading capitalisation is grading the
	// wrong thing.
	if g := grade(c, req, textResp("the capital is paris")); !g.Passed {
		t.Errorf("case-sensitive match: %v", g.Failures)
	}
	if g := grade(c, req, textResp("The capital is Lyon.")); g.Passed {
		t.Error("passed a case that missed the expected answer")
	}
}

func TestGradeReportsEveryFailure(t *testing.T) {
	// Not just the first. A model that missed three things is a different signal
	// from one that missed one, and re-running to discover the second failure
	// costs another provider call.
	c := caseWith(Expect{Contains: []string{"alpha", "beta", "gamma"}})
	g := grade(c, c.Request(0), textResp("only alpha here"))

	if g.Passed {
		t.Fatal("passed")
	}
	if len(g.Failures) != 2 {
		t.Errorf("reported %d failures, want 2: %v", len(g.Failures), g.Failures)
	}
}

func TestGradeToolCall(t *testing.T) {
	c := caseWith(Expect{CallsTool: "get_weather", ToolArgs: map[string]string{"city": "Reykjavik"}})
	c.Tools = []ToolSpec{{Name: "get_weather"}}
	req := c.Request(0)

	call := func(name, args string) *provider.Response {
		return &provider.Response{
			Parts:        []domain.ContentPart{{Kind: domain.PartToolCall, ToolName: name, Arguments: args}},
			FinishReason: provider.FinishStop,
		}
	}

	if g := grade(c, req, call("get_weather", `{"city":"Reykjavik"}`)); !g.Passed {
		t.Errorf("failed a correct tool call: %v", g.Failures)
	}
	if g := grade(c, req, call("get_weather", `{"city":"Oslo"}`)); g.Passed {
		t.Error("passed a tool call with the wrong argument")
	}
	if g := grade(c, req, textResp("It is cold in Reykjavik.")); g.Passed {
		t.Error("passed a case that never called the tool")
	}
}

func TestGradeJSONPath(t *testing.T) {
	c := caseWith(Expect{JSONPath: map[string]string{
		"name":         "Dana",
		"age":          "34",
		"tags[1]":      "b",
		"address.city": "Lagos",
	}})
	c.ResponseFormat = "json_object"
	req := c.Request(0)

	body := `{"name":"Dana","age":34,"tags":["a","b"],"address":{"city":"Lagos"}}`
	if g := grade(c, req, textResp(body)); !g.Passed {
		t.Errorf("failed a conforming document: %v", g.Failures)
	}

	// The number comparison is the one worth pinning. Every expectation in a
	// YAML file is a string, and JSON decodes 34 to a float64 — so "34" must
	// match 34, or every numeric expectation in every suite fails.
	if g := grade(c, req, textResp(`{"name":"Dana","age":"34","tags":["a","b"],"address":{"city":"Lagos"}}`)); !g.Passed {
		t.Errorf("a string 34 did not match the expectation: %v", g.Failures)
	}
	if g := grade(c, req, textResp(`{"name":"Dana","age":35,"tags":["a","b"],"address":{"city":"Lagos"}}`)); g.Passed {
		t.Error("passed a document with the wrong value")
	}
}

func TestGradeUsesTheSameValidityCheckAsProduction(t *testing.T) {
	// Deliberately shared with the request path's cascade escalation. Two
	// definitions of "invalid" would let a model score well on evals while
	// escalating constantly in production, which is the exact gap the eval
	// exists to close.
	c := caseWith(Expect{Contains: []string{"anything"}})
	g := grade(c, c.Request(0), &provider.Response{FinishReason: provider.FinishStop})

	if g.Passed {
		t.Fatal("an empty completion passed")
	}
	if !strings.Contains(g.Failures[0], "invalid response") {
		t.Errorf("failure = %q, want it to name the validity check", g.Failures[0])
	}
}

// --- scoring ---

func TestScoresAreWeightedPassRatesPerDimension(t *testing.T) {
	r := EndpointResult{Cases: []CaseResult{
		{Dimension: "coding", Weight: 1, Runs: 4, Passed: 3},
		{Dimension: "coding", Weight: 1, Runs: 4, Passed: 1},
		{Dimension: "tools", Weight: 1, Runs: 2, Passed: 2},
	}}

	scores := r.Scores()
	if got := scores["coding"]; got != 0.5 {
		t.Errorf("coding = %v, want 0.5 (4 of 8)", got)
	}
	if got := scores["tools"]; got != 1 {
		t.Errorf("tools = %v, want 1", got)
	}
}

func TestWeightsCountWithinADimension(t *testing.T) {
	// A reading-comprehension failure should cost more than a stylistic one, and
	// the suite says so per case rather than by repeating a case three times.
	r := EndpointResult{Cases: []CaseResult{
		{Dimension: "coding", Weight: 3, Runs: 1, Passed: 0},
		{Dimension: "coding", Weight: 1, Runs: 1, Passed: 1},
	}}
	if got := r.Scores()["coding"]; got != 0.25 {
		t.Errorf("coding = %v, want 0.25", got)
	}
}

func TestErroredRunsDoNotLowerAScore(t *testing.T) {
	// A provider outage during an eval is not evidence about the model. Folding
	// it into the score would revise a model's quality downward because a
	// network was flaky that afternoon — and that revision would then be pasted
	// into a catalog and quoted to a customer.
	r := EndpointResult{Cases: []CaseResult{
		{Dimension: "coding", Weight: 1, Runs: 10, Passed: 2, Errors: 8},
	}}
	if got := r.Scores()["coding"]; got != 1 {
		t.Errorf("coding = %v, want 1: two of two completed runs passed", got)
	}
	if got := r.Errored(); got != 8 {
		t.Errorf("Errored = %d, want 8 — excluded from the score, not from the report", got)
	}
}

func TestAllErroredCaseContributesNothing(t *testing.T) {
	r := EndpointResult{Cases: []CaseResult{
		{Dimension: "coding", Weight: 1, Runs: 3, Passed: 0, Errors: 3},
		{Dimension: "tools", Weight: 1, Runs: 2, Passed: 2},
	}}
	scores := r.Scores()
	if _, present := scores["coding"]; present {
		t.Error("a dimension where every run errored produced a score anyway")
	}
	if scores["tools"] != 1 {
		t.Errorf("tools = %v, want 1", scores["tools"])
	}
}

// --- runner ---

// scriptedAdapter answers with a fixed sequence, so the runner can be exercised
// without a provider.
type scriptedAdapter struct {
	replies []*provider.Response
	errs    []error
	calls   int
}

func (a *scriptedAdapter) ID() domain.ProviderID { return "openai" }

func (a *scriptedAdapter) Chat(context.Context, *domain.NormalizedRequest, *domain.ModelEndpoint, provider.Credential) (*provider.Response, error) {
	i := a.calls
	a.calls++
	if i < len(a.errs) && a.errs[i] != nil {
		return nil, a.errs[i]
	}
	if i < len(a.replies) {
		return a.replies[i], nil
	}
	return a.replies[len(a.replies)-1], nil
}

func (a *scriptedAdapter) ChatStream(context.Context, *domain.NormalizedRequest, *domain.ModelEndpoint, provider.Credential) (provider.Stream, error) {
	return nil, nil
}

func (a *scriptedAdapter) ClassifyError(r *http.Response, err error) provider.ErrorClass {
	return provider.DefaultClassify(r, err)
}

func TestRunnerRepeatsAndScores(t *testing.T) {
	adapter := &scriptedAdapter{replies: []*provider.Response{
		textResp("Paris"), textResp("Lyon"), textResp("Paris"),
	}}
	runner := &Runner{
		Registry: provider.NewRegistry(adapter),
		Resolver: provider.StaticResolver{"primary": {Ref: "primary"}},
	}
	suite := &Suite{
		Repeats: 3,
		Cases:   []Case{caseWith(Expect{Contains: []string{"Paris"}})},
	}
	ep := &domain.ModelEndpoint{
		ID: "e", Provider: "openai", Model: "m", CredentialRef: "primary",
		Pricing: domain.Pricing{Input: 1_000_000, Output: 1_000_000},
	}

	res, err := runner.Run(t.Context(), suite, ep)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if adapter.calls != 3 {
		t.Errorf("made %d calls, want 3", adapter.calls)
	}
	// Two of three, which is the point of repeating: a model that answers
	// correctly two times in three is a different proposition from one that
	// always does, and one run cannot tell them apart.
	if got := res.Scores()["coding"]; got < 0.66 || got > 0.67 {
		t.Errorf("score = %v, want 2/3", got)
	}
}

func TestRunnerSeparatesCallFailuresFromWrongAnswers(t *testing.T) {
	adapter := &scriptedAdapter{
		replies: []*provider.Response{textResp("Paris"), nil, textResp("Paris")},
		errs:    []error{nil, &provider.Error{Class: provider.ClassRetrySame, Message: "503"}, nil},
	}
	runner := &Runner{
		Registry: provider.NewRegistry(adapter),
		Resolver: provider.StaticResolver{"primary": {Ref: "primary"}},
	}
	suite := &Suite{Repeats: 3, Cases: []Case{caseWith(Expect{Contains: []string{"Paris"}})}}
	ep := &domain.ModelEndpoint{ID: "e", Provider: "openai", Model: "m", CredentialRef: "primary"}

	res, err := runner.Run(t.Context(), suite, ep)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Cases[0].Errors != 1 {
		t.Errorf("Errors = %d, want 1", res.Cases[0].Errors)
	}
	if got := res.Scores()["coding"]; got != 1 {
		t.Errorf("score = %v, want 1: both completed runs answered correctly", got)
	}
}
