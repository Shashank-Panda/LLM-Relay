package domain

import (
	"reflect"
	"testing"
)

func sampleRequest() *NormalizedRequest {
	return &NormalizedRequest{
		ID:        "req-1",
		Tenant:    "t",
		RouteName: "relay/r",
		Baseline:  Baseline{EndpointID: "p/a@r", Mode: ModeOptimize, Source: "explicit_model"},
		System:    []ContentPart{{Kind: PartText, Text: "be helpful"}},
		Messages: []Message{
			{Role: RoleUser, Parts: []ContentPart{{Kind: PartText, Text: "hello"}}},
		},
		Tools: []ToolDef{{Name: "search", Schema: `{"type":"object"}`}},
		Params: SamplingParams{
			Temperature: Ptr(0.7),
			MaxTokens:   Ptr(1024),
			Stop:        []string{"\n\n"},
		},
		SessionKey:       "s",
		PreviousEndpoint: "p/a@r",
		Metadata:         map[string]string{"k": "v"},
	}
}

// TestCloneIsDeep is the property the optimizer's no-mutation guarantee rests
// on. A shallow copy would be silently wrong: setting a cache breakpoint writes
// into a ContentPart inside a slice the original still shares.
func TestCloneIsDeep(t *testing.T) {
	orig := sampleRequest()
	before := sampleRequest()

	c := orig.Clone()

	c.System[0].CacheBreakpoint = true
	c.System[0].Text = "changed"
	c.Messages[0].Parts[0].CacheBreakpoint = true
	c.Messages[0].Parts[0].Text = "changed"
	c.Tools[0].CacheBreakpoint = true
	c.Tools[0].Name = "changed"
	*c.Params.Temperature = 0.1
	*c.Params.MaxTokens = 1
	c.Params.Stop[0] = "changed"
	c.Metadata["k"] = "changed"
	c.Params.ReasoningEffort = Ptr(EffortHigh)

	if !reflect.DeepEqual(orig, before) {
		t.Error("mutating the clone reached back into the original")
	}
}

func TestCloneNil(t *testing.T) {
	var r *NormalizedRequest
	if r.Clone() != nil {
		t.Error("Clone on nil should return nil")
	}
}

func TestNeedsDerivation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*NormalizedRequest)
		want   Need
	}{
		{
			name:   "tools present",
			mutate: func(*NormalizedRequest) {},
			want:   Need{Tools: true},
		},
		{
			name:   "no tools",
			mutate: func(r *NormalizedRequest) { r.Tools = nil },
			want:   Need{},
		},
		{
			name: "image in a message",
			mutate: func(r *NormalizedRequest) {
				r.Messages[0].Parts = append(r.Messages[0].Parts,
					ContentPart{Kind: PartImage, URL: "https://example.test/x.png"})
			},
			want: Need{Tools: true, Vision: true},
		},
		{
			name: "image in the system prompt",
			mutate: func(r *NormalizedRequest) {
				r.System = append(r.System, ContentPart{Kind: PartImage, URL: "x"})
			},
			want: Need{Tools: true, Vision: true},
		},
		{
			name: "json schema",
			mutate: func(r *NormalizedRequest) {
				r.ResponseFormat = ResponseFormat{Type: FormatJSONSchema, Schema: "{}"}
			},
			want: Need{Tools: true, JSONSchema: true},
		},
		{
			name:   "json object is not a schema requirement",
			mutate: func(r *NormalizedRequest) { r.ResponseFormat = ResponseFormat{Type: FormatJSONObject} },
			want:   Need{Tools: true},
		},
		{
			name:   "streaming",
			mutate: func(r *NormalizedRequest) { r.Stream = true },
			want:   Need{Tools: true, Streaming: true},
		},
		{
			name: "caller-set effort is a requirement",
			mutate: func(r *NormalizedRequest) {
				r.Params.ReasoningEffort = Ptr(EffortHigh)
			},
			want: Need{Tools: true, Reasoning: true},
		},
		{
			// The seam that matters: a Relay-applied default must not narrow
			// the candidate set, or enabling the lever would eliminate every
			// non-reasoning endpoint from every route.
			name: "optimizer-applied effort is not a requirement",
			mutate: func(r *NormalizedRequest) {
				r.Params.ReasoningEffort = Ptr(EffortLow)
				r.Params.EffortFromOptimizer = true
			},
			want: Need{Tools: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleRequest()
			tc.mutate(r)
			if got := r.Needs(); got != tc.want {
				t.Errorf("Needs() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRoutingViewCarriesRoutingFields(t *testing.T) {
	r := sampleRequest()
	r.Estimate = Estimate{InputTokens: 100, MaxOutputTokens: 200, ExpectedOutputTokens: 50}

	v := r.RoutingView()

	if v.ID != r.ID || v.Tenant != r.Tenant || v.RouteName != r.RouteName {
		t.Error("identity fields did not carry across")
	}
	if v.Baseline != r.Baseline {
		t.Errorf("Baseline = %+v, want %+v", v.Baseline, r.Baseline)
	}
	if v.Estimate != r.Estimate {
		t.Errorf("Estimate = %+v, want %+v", v.Estimate, r.Estimate)
	}
	if v.SessionKey != r.SessionKey || v.PreviousEndpoint != r.PreviousEndpoint {
		t.Error("session fields did not carry across")
	}
	if v.Need != r.Needs() {
		t.Error("Need was not derived")
	}
}

func TestApproxTokens(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"abcdefgh", 2},
	}
	for _, tc := range tests {
		if got := ApproxTokens(tc.in); got != tc.want {
			t.Errorf("ApproxTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPartApproxTokens(t *testing.T) {
	t.Run("image is a flat estimate", func(t *testing.T) {
		// Image tokenization is provider- and resolution-specific and cannot be
		// derived from a URL, so a flat number is honest where a computed one
		// would only look precise.
		p := ContentPart{Kind: PartImage, URL: "https://example.test/tiny.png"}
		if got := p.ApproxTokens(); got != approxImageTokens {
			t.Errorf("image tokens = %d, want %d", got, approxImageTokens)
		}
	})

	t.Run("tool call counts its arguments", func(t *testing.T) {
		p := ContentPart{Kind: PartToolCall, ToolName: "search", Arguments: `{"query":"golang"}`}
		if p.ApproxTokens() <= ApproxTokens(p.ToolName) {
			t.Error("tool call ignored its arguments")
		}
	})
}

func TestMessageHelpers(t *testing.T) {
	m := Message{Role: RoleAssistant, Parts: []ContentPart{
		{Kind: PartText, Text: "abcd"},
		{Kind: PartToolCall, ToolName: "x"},
	}}

	if !m.HasKind(PartToolCall) {
		t.Error("HasKind missed a present kind")
	}
	if m.HasKind(PartImage) {
		t.Error("HasKind found an absent kind")
	}
	if m.ApproxTokens() <= 4 {
		t.Error("message tokens should include framing overhead plus parts")
	}
}

func TestReasoningEffortOrdering(t *testing.T) {
	if EffortMinimal.Level() >= EffortLow.Level() ||
		EffortLow.Level() >= EffortMedium.Level() ||
		EffortMedium.Level() >= EffortHigh.Level() {
		t.Error("effort levels are not strictly ordered")
	}
	for _, e := range []ReasoningEffort{EffortMinimal, EffortLow, EffortMedium, EffortHigh} {
		if !e.Valid() {
			t.Errorf("%q reported invalid", e)
		}
	}
	if ReasoningEffort("enthusiastic").Valid() {
		t.Error("an unrecognised effort reported valid")
	}
	if ReasoningEffort("").Valid() {
		t.Error("empty effort reported valid")
	}
}

func TestInputTokensCountsEverything(t *testing.T) {
	r := sampleRequest()
	base := r.InputTokens()

	r.Tools = nil
	if noTools := r.InputTokens(); noTools >= base {
		t.Error("tool definitions were not counted")
	}

	r = sampleRequest()
	r.System = nil
	if noSystem := r.InputTokens(); noSystem >= base {
		t.Error("system prompt was not counted")
	}

	r = sampleRequest()
	r.Messages = nil
	if noMessages := r.InputTokens(); noMessages >= base {
		t.Error("messages were not counted")
	}
}

func TestRecommendedLeversAreTheSafeOnes(t *testing.T) {
	c := RecommendedLevers()

	if !c.CacheBreakpoints || !c.OutputCeiling {
		t.Error("the semantics-preserving levers should be on")
	}
	// Pruning changes what the model is asked; effort is a behaviour change on
	// reasoning models. Neither rides along with the safe set.
	if c.ContextPruning || c.EffortDownshift {
		t.Error("a lever that changes model behaviour is on by default")
	}
	if c.OutputCeilingSlack < 1 {
		t.Error("slack below 1 would truncate the majority of responses")
	}
}

func TestZeroLeverConfigEnablesNothing(t *testing.T) {
	var c LeverConfig
	if c.CacheBreakpoints || c.OutputCeiling || c.ContextPruning || c.EffortDownshift {
		t.Error("the zero LeverConfig enables an optimization")
	}
}
