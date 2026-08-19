package execute

import (
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/validate"
)

// answer builds a response with text and reported usage.
func answer(text string, in, out int) *provider.Response {
	r := &provider.Response{
		FinishReason: provider.FinishStop,
		Usage:        provider.Usage{InputTokens: in, OutputTokens: out},
	}
	if text != "" {
		r.Parts = []domain.ContentPart{{Kind: domain.PartText, Text: text}}
	}
	return r
}

// downgraded is the decision shape escalation applies to: the baseline is one
// endpoint and something cheaper was chosen.
func downgraded() *domain.Decision {
	return &domain.Decision{
		Chosen:   "cheap",
		Baseline: domain.Baseline{EndpointID: "dear", Mode: domain.ModeOptimize},
		Ranked:   []domain.ScoredCandidate{{EndpointID: "cheap"}, {EndpointID: "dear"}},
		Failover: []string{"dear"},
	}
}

func escalationRun(t *testing.T, a *scriptedAdapter, req *domain.NormalizedRequest, d *domain.Decision) Result {
	t.Helper()
	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: req, Catalog: catalog("cheap", "dear"), Decision: d, Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestEscalatesOnInvalidOutput(t *testing.T) {
	a := newScripted("openai").
		script("cheap", step{resp: answer("", 100, 0)}). // empty: invalid
		script("dear", step{resp: answer("a real answer", 100, 50)})

	res := escalationRun(t, a, &domain.NormalizedRequest{}, downgraded())

	if res.Escalation == nil {
		t.Fatal("no escalation on an empty completion")
	}
	if res.Escalation.From != "cheap" || res.Escalation.To != "dear" {
		t.Errorf("escalation = %+v, want cheap -> dear", res.Escalation)
	}
	if res.Escalation.Reason != validate.ReasonEmpty {
		t.Errorf("reason = %q, want %q", res.Escalation.Reason, validate.ReasonEmpty)
	}
	if !res.Escalation.Recovered {
		t.Error("the baseline's answer was valid but Recovered is false")
	}
	// The client gets the good answer, which is the entire point.
	if got := res.Response.Parts[0].Text; got != "a real answer" {
		t.Errorf("response = %q, want the baseline's", got)
	}
	if got := a.called(); !equal(got, []string{"cheap", "dear"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestDiscardedAttemptIsCharged(t *testing.T) {
	// The uncomfortable arithmetic that makes the ledger a measurement rather
	// than marketing: the customer paid for two answers and used one, and the
	// wasted one is recorded.
	a := newScripted("openai").
		script("cheap", step{resp: answer("", 1000, 200)}).
		script("dear", step{resp: answer("fine", 1000, 300)})

	res := escalationRun(t, a, &domain.NormalizedRequest{}, downgraded())

	if len(res.Discarded) != 1 {
		t.Fatalf("Discarded = %+v, want the abandoned attempt", res.Discarded)
	}
	if res.Discarded[0].EndpointID != "cheap" {
		t.Errorf("discarded endpoint = %q, want cheap", res.Discarded[0].EndpointID)
	}
	if got := res.Discarded[0].Usage.OutputTokens; got != 200 {
		t.Errorf("discarded output tokens = %d, want 200", got)
	}
	if got := res.DiscardedUsage(); got != 1200 {
		t.Errorf("DiscardedUsage = %d, want 1200", got)
	}
}

func TestValidOutputIsNotEscalated(t *testing.T) {
	// The common path, and the one that must be cheap: every downgraded request
	// runs this check and almost none of them escalate.
	a := newScripted("openai").script("cheap", step{resp: answer("a good answer", 10, 5)})

	res := escalationRun(t, a, &domain.NormalizedRequest{}, downgraded())

	if res.Escalation != nil {
		t.Errorf("escalated a valid response: %+v", res.Escalation)
	}
	if got := a.called(); !equal(got, []string{"cheap"}) {
		t.Errorf("calls = %v, want one", got)
	}
}

func TestBaselineIsNotEscalatedToItself(t *testing.T) {
	// Nothing was downgraded, so there is nowhere better to go. A second
	// identical call produces an identically invalid answer often enough that
	// it is not worth paying to find out.
	d := &domain.Decision{
		Chosen:   "dear",
		Baseline: domain.Baseline{EndpointID: "dear", Mode: domain.ModeStrict},
	}
	a := newScripted("openai").script("dear", step{resp: answer("", 10, 0)})

	res := escalationRun(t, a, &domain.NormalizedRequest{}, d)

	if res.Escalation != nil {
		t.Error("escalated from the baseline to the baseline")
	}
	if got := len(a.called()); got != 1 {
		t.Errorf("made %d calls, want 1", got)
	}
}

func TestEscalationHappensOnce(t *testing.T) {
	// Both endpoints return invalid output. A second escalation would mean the
	// baseline is also producing invalid answers, and retrying further spends
	// money to discover that the request is the problem.
	a := newScripted("openai").
		script("cheap", step{resp: answer("", 10, 0)}).
		script("dear", step{resp: answer("", 10, 0)})

	res := escalationRun(t, a, &domain.NormalizedRequest{}, downgraded())

	if got := a.called(); !equal(got, []string{"cheap", "dear"}) {
		t.Errorf("calls = %v, want exactly one escalation", got)
	}
	if res.Escalation == nil {
		t.Fatal("no escalation recorded")
	}
	// Recorded as not recovered, which is a signal about the request rather
	// than about either endpoint.
	if res.Escalation.Recovered {
		t.Error("Recovered = true when the baseline's answer was also invalid")
	}
}

func TestStreamingIsNotEscalated(t *testing.T) {
	// ADR-0009's stated limitation, pinned so it is not discovered as a
	// surprise. The checks that matter need the complete response, and by then
	// the client has already received most of it — past the first flushed byte
	// there is no honest way to replace what was sent (ADR-0003).
	a := newScripted("openai")

	res, err := newExecutor(a).Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: catalog("cheap", "dear"),
		Decision: downgraded(), Streaming: true, Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Stream.Close()

	if res.Escalation != nil {
		t.Error("escalated a stream")
	}
}

func TestUnreachableBaselineReturnsTheInvalidAnswer(t *testing.T) {
	// Fail open. The downgraded answer is invalid, but it is what exists, and
	// returning it beats failing a request that has an answer — the caller can
	// judge it, and a 502 gives them nothing to judge.
	cat := catalog("cheap", "dear")
	cat.Endpoints["dear"].CredentialRef = ""

	a := newScripted("openai").script("cheap", step{resp: answer("", 10, 0)})

	var degradedReasons []string
	e := newExecutor(a).WithDegradedHook(func(_, reason string) {
		degradedReasons = append(degradedReasons, reason)
	})

	res, err := e.Run(t.Context(), Request{
		Request: &domain.NormalizedRequest{}, Catalog: cat,
		Decision: downgraded(), Policy: testPolicy(),
	})
	if err != nil {
		t.Fatalf("Run: %v — an unreachable baseline must not fail the request", err)
	}
	if res.Response == nil {
		t.Fatal("no response returned")
	}
	if res.Escalation == nil {
		t.Error("the attempted escalation was not recorded")
	}
	// And it is counted, because a fail-open path nobody counts is one nobody
	// knows is being taken.
	if len(degradedReasons) == 0 {
		t.Error("the degradation was not reported")
	}
}

func TestEscalationTriggers(t *testing.T) {
	// The full trigger matrix, end to end through the executor rather than
	// against the checker directly: what matters is that each of these actually
	// produces a second call.
	jsonReq := func() *domain.NormalizedRequest {
		r := &domain.NormalizedRequest{}
		r.ResponseFormat = domain.ResponseFormat{Type: domain.FormatJSONObject}
		return r
	}
	toolReq := func() *domain.NormalizedRequest {
		return &domain.NormalizedRequest{Tools: []domain.ToolDef{{Name: "search"}}}
	}
	toolResp := func(name, args string) *provider.Response {
		return &provider.Response{
			Parts: []domain.ContentPart{{
				Kind: domain.PartToolCall, ToolName: name, Arguments: args,
			}},
			FinishReason: provider.FinishStop,
		}
	}

	tests := map[string]struct {
		req  *domain.NormalizedRequest
		bad  *provider.Response
		want validate.Reason
	}{
		"empty":           {&domain.NormalizedRequest{}, answer("", 1, 0), validate.ReasonEmpty},
		"invalid json":    {jsonReq(), answer("here you go: {oops", 1, 1), validate.ReasonInvalidJSON},
		"bad tool args":   {toolReq(), toolResp("search", "{broken"), validate.ReasonInvalidToolCall},
		"undeclared tool": {toolReq(), toolResp("delete_everything", "{}"), validate.ReasonUndeclaredTool},
		"refusal": {&domain.NormalizedRequest{}, &provider.Response{
			Parts:        []domain.ContentPart{{Kind: domain.PartText, Text: "no"}},
			FinishReason: provider.FinishContentFilter,
		}, validate.ReasonRefusal},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			a := newScripted("openai").
				script("cheap", step{resp: tc.bad}).
				script("dear", step{resp: answer("recovered", 1, 1)})

			res := escalationRun(t, a, tc.req, downgraded())

			if res.Escalation == nil {
				t.Fatalf("no escalation for %s", name)
			}
			if res.Escalation.Reason != tc.want {
				t.Errorf("reason = %q, want %q", res.Escalation.Reason, tc.want)
			}
		})
	}
}
