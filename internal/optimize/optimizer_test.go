package optimize

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// --- fixtures ---

func text(s string) domain.ContentPart {
	return domain.ContentPart{Kind: domain.PartText, Text: s}
}

// filler returns a string of roughly n tokens under the 4-bytes-per-token
// heuristic, so tests can express "long enough to be worth caching".
func filler(tokens int) string { return strings.Repeat("x", tokens*4) }

func msg(role domain.Role, s string) domain.Message {
	return domain.Message{Role: role, Parts: []domain.ContentPart{text(s)}}
}

func baseRequest() *domain.NormalizedRequest {
	return &domain.NormalizedRequest{
		ID:        "req-1",
		Tenant:    "t",
		RouteName: "relay/fast-coder",
		System:    []domain.ContentPart{text(filler(2000))},
		Tools: []domain.ToolDef{
			{Name: "search", Description: filler(500), Schema: filler(500)},
		},
		Messages: []domain.Message{
			msg(domain.RoleUser, filler(1500)),
			msg(domain.RoleAssistant, filler(1500)),
			msg(domain.RoleUser, filler(1500)),
		},
	}
}

func statsWith(p95 int) *Stats {
	return &Stats{OutputTokensP95: map[string]int{"relay/fast-coder": p95}}
}

func find(ops []domain.Optimization, lever string) (domain.Optimization, bool) {
	for _, o := range ops {
		if o.Lever == lever {
			return o, true
		}
	}
	return domain.Optimization{}, false
}

func breakpointCount(r *domain.NormalizedRequest) int {
	n := 0
	for _, p := range r.System {
		if p.CacheBreakpoint {
			n++
		}
	}
	for _, t := range r.Tools {
		if t.CacheBreakpoint {
			n++
		}
	}
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			if p.CacheBreakpoint {
				n++
			}
		}
	}
	return n
}

// --- core contracts ---

func TestZeroConfigDoesNothing(t *testing.T) {
	// The same posture as strict mode: nobody's requests get rewritten because
	// a config field was left blank.
	in := baseRequest()
	out, ops := New(domain.LeverConfig{}).Apply(in, statsWith(300))

	if len(ops) != 0 {
		t.Errorf("zero config applied %d optimizations: %+v", len(ops), ops)
	}
	if breakpointCount(out) != 0 {
		t.Error("zero config placed cache breakpoints")
	}
	if out.Params.MaxTokens != nil {
		t.Error("zero config set max_tokens")
	}
	if out.Params.ReasoningEffort != nil {
		t.Error("zero config set reasoning effort")
	}
}

func TestApplyNeverMutatesTheInput(t *testing.T) {
	// The original must survive for retries, failover, and the baseline cost
	// comparison — and so that "what did the caller actually send" stays
	// answerable after the fact.
	in := baseRequest()
	before := in.Clone()

	cfg := domain.RecommendedLevers()
	cfg.EffortDownshift = true
	cfg.DefaultEffort = domain.EffortLow
	cfg.ContextPruning = true
	cfg.KeepTurns = 1

	New(cfg).Apply(in, statsWith(300))

	if !reflect.DeepEqual(in, before) {
		t.Error("Apply mutated its input")
	}
}

func TestNilRequest(t *testing.T) {
	out, ops := New(domain.RecommendedLevers()).Apply(nil, nil)
	if out != nil || ops != nil {
		t.Errorf("Apply(nil) = (%v, %v), want (nil, nil)", out, ops)
	}
}

func TestDeterminism(t *testing.T) {
	cfg := domain.RecommendedLevers()
	cfg.EffortDownshift = true
	cfg.DefaultEffort = domain.EffortLow
	o := New(cfg)

	want, wantOps := o.Apply(baseRequest(), statsWith(300))
	for i := range 200 {
		got, gotOps := o.Apply(baseRequest(), statsWith(300))
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotOps, wantOps) {
			t.Fatalf("iteration %d differed", i)
		}
	}
}

// --- cache breakpoints ---

func TestCacheBreakpoints(t *testing.T) {
	cfg := domain.RecommendedLevers()

	t.Run("marks the stable prefixes", func(t *testing.T) {
		out, ops := New(cfg).Apply(baseRequest(), nil)

		op, ok := find(ops, domain.LeverCacheBreakpoints)
		if !ok {
			t.Fatal("no cache breakpoint optimization recorded")
		}
		if breakpointCount(out) == 0 {
			t.Fatal("optimization recorded but nothing was marked")
		}
		if op.After != "3" {
			t.Errorf("marked %s prefixes, want 3 (tools, history, full)", op.After)
		}

		// The tool block is the anchor every request in the tenant reuses.
		if !out.Tools[len(out.Tools)-1].CacheBreakpoint {
			t.Error("tool definitions were not marked")
		}
		// The history anchor is what pays for multi-turn conversation.
		hist := out.Messages[len(out.Messages)-2].Parts
		if !hist[len(hist)-1].CacheBreakpoint {
			t.Error("conversation history was not marked")
		}
	})

	t.Run("a short prompt is not worth caching", func(t *testing.T) {
		req := &domain.NormalizedRequest{
			RouteName: "relay/fast-coder",
			System:    []domain.ContentPart{text("be brief")},
			Messages:  []domain.Message{msg(domain.RoleUser, "hi")},
		}

		out, ops := New(cfg).Apply(req, nil)

		if _, ok := find(ops, domain.LeverCacheBreakpoints); ok {
			t.Error("marked a prompt far below the cacheable minimum")
		}
		if breakpointCount(out) != 0 {
			t.Error("placed a breakpoint on a trivial prompt")
		}
	})

	t.Run("the gap rule suppresses redundant markers", func(t *testing.T) {
		// A large history followed by a tiny final message: the full-prefix
		// anchor adds almost nothing over the history anchor, and every marker
		// costs a cache write.
		req := baseRequest()
		req.Messages = append(req.Messages, msg(domain.RoleUser, "ok"))

		out, _ := New(cfg).Apply(req, nil)

		last := out.Messages[len(out.Messages)-1].Parts
		if last[len(last)-1].CacheBreakpoint {
			t.Error("marked a final message that adds no cacheable tokens")
		}
	})

	t.Run("respects MaxBreakpoints", func(t *testing.T) {
		c := cfg
		c.MaxBreakpoints = 1

		out, _ := New(c).Apply(baseRequest(), nil)

		if got := breakpointCount(out); got != 1 {
			t.Errorf("placed %d breakpoints, want 1", got)
		}
	})

	t.Run("system prompt anchors when there are no tools", func(t *testing.T) {
		req := baseRequest()
		req.Tools = nil

		out, _ := New(cfg).Apply(req, nil)

		if !out.System[len(out.System)-1].CacheBreakpoint {
			t.Error("system prompt was not marked in the absence of tools")
		}
	})

	t.Run("no messages", func(t *testing.T) {
		req := &domain.NormalizedRequest{
			RouteName: "relay/fast-coder",
			System:    []domain.ContentPart{text(filler(3000))},
		}
		out, _ := New(cfg).Apply(req, nil)
		if breakpointCount(out) != 1 {
			t.Errorf("breakpoints = %d, want 1", breakpointCount(out))
		}
	})
}

// --- reasoning effort ---

func TestDefaultEffort(t *testing.T) {
	cfg := domain.RecommendedLevers()
	cfg.EffortDownshift = true
	cfg.DefaultEffort = domain.EffortLow

	t.Run("fills in an unset effort", func(t *testing.T) {
		out, ops := New(cfg).Apply(baseRequest(), nil)

		if _, ok := find(ops, domain.LeverEffort); !ok {
			t.Fatal("no effort optimization recorded")
		}
		if out.Params.ReasoningEffort == nil || *out.Params.ReasoningEffort != domain.EffortLow {
			t.Errorf("effort = %v, want low", out.Params.ReasoningEffort)
		}
		if !out.Params.EffortFromOptimizer {
			t.Error("EffortFromOptimizer = false; provenance was not recorded")
		}
	})

	t.Run("never overrides an explicit effort", func(t *testing.T) {
		// Lowering an effort the caller chose contradicts a stated decision,
		// which is a different act from supplying a default for a blank field.
		req := baseRequest()
		req.Params.ReasoningEffort = domain.Ptr(domain.EffortHigh)

		out, ops := New(cfg).Apply(req, nil)

		if _, ok := find(ops, domain.LeverEffort); ok {
			t.Error("optimizer overrode a caller-set effort")
		}
		if *out.Params.ReasoningEffort != domain.EffortHigh {
			t.Errorf("effort = %v, want high", *out.Params.ReasoningEffort)
		}
		if out.Params.EffortFromOptimizer {
			t.Error("a caller-set effort was marked as optimizer-applied")
		}
	})

	t.Run("a defaulted effort does not narrow the candidate set", func(t *testing.T) {
		// The trap this guards: if a Relay-applied default counted as a
		// capability requirement, enabling the lever would silently eliminate
		// every non-reasoning endpoint from every route.
		out, _ := New(cfg).Apply(baseRequest(), nil)

		if out.Needs().Reasoning {
			t.Error("an optimizer-applied effort became a routing requirement")
		}
	})

	t.Run("a caller-set effort is a requirement", func(t *testing.T) {
		req := baseRequest()
		req.Params.ReasoningEffort = domain.Ptr(domain.EffortHigh)

		out, _ := New(cfg).Apply(req, nil)

		if !out.Needs().Reasoning {
			t.Error("a caller-set effort did not become a routing requirement")
		}
	})

	t.Run("an invalid configured default is ignored", func(t *testing.T) {
		c := cfg
		c.DefaultEffort = "enthusiastic"

		out, ops := New(c).Apply(baseRequest(), nil)

		if _, ok := find(ops, domain.LeverEffort); ok {
			t.Error("applied an unrecognised effort level")
		}
		if out.Params.ReasoningEffort != nil {
			t.Error("set an unrecognised effort level")
		}
	})
}

// --- output ceiling ---

func TestOutputCeiling(t *testing.T) {
	cfg := domain.RecommendedLevers() // slack 1.5, min 512, max 16384

	t.Run("derives a ceiling from observed p95", func(t *testing.T) {
		out, ops := New(cfg).Apply(baseRequest(), statsWith(4000))

		op, ok := find(ops, domain.LeverMaxTokens)
		if !ok {
			t.Fatal("no max_tokens optimization recorded")
		}
		if out.Params.MaxTokens == nil || *out.Params.MaxTokens != 6000 {
			t.Errorf("max_tokens = %v, want 6000 (4000 p95 x 1.5)", out.Params.MaxTokens)
		}
		if op.Before != "unset" {
			t.Errorf("Before = %q, want %q", op.Before, "unset")
		}
	})

	t.Run("never overrides an explicit max_tokens", func(t *testing.T) {
		req := baseRequest()
		req.Params.MaxTokens = domain.Ptr(64000)

		out, ops := New(cfg).Apply(req, statsWith(300))

		if _, ok := find(ops, domain.LeverMaxTokens); ok {
			t.Error("optimizer overrode a caller-set max_tokens")
		}
		if *out.Params.MaxTokens != 64000 {
			t.Errorf("max_tokens = %d, want 64000", *out.Params.MaxTokens)
		}
	})

	t.Run("declines without history", func(t *testing.T) {
		// No basis is no basis. Inventing a ceiling here would truncate
		// responses on a route nobody has measured.
		out, ops := New(cfg).Apply(baseRequest(), nil)

		if _, ok := find(ops, domain.LeverMaxTokens); ok {
			t.Error("set a ceiling with no observed history")
		}
		if out.Params.MaxTokens != nil {
			t.Error("max_tokens was set with no observed history")
		}
	})

	t.Run("clamps to the configured floor", func(t *testing.T) {
		out, _ := New(cfg).Apply(baseRequest(), statsWith(100))
		if *out.Params.MaxTokens != 512 {
			t.Errorf("max_tokens = %d, want the 512 floor", *out.Params.MaxTokens)
		}
	})

	t.Run("clamps to the configured ceiling", func(t *testing.T) {
		out, _ := New(cfg).Apply(baseRequest(), statsWith(100000))
		if *out.Params.MaxTokens != 16384 {
			t.Errorf("max_tokens = %d, want the 16384 cap", *out.Params.MaxTokens)
		}
	})

	t.Run("slack below 1 is treated as no slack", func(t *testing.T) {
		// A misconfiguration that would truncate the majority of responses is
		// clamped rather than acted on.
		c := cfg
		c.OutputCeilingSlack = 0.5
		c.MinOutputCeiling = 0

		out, _ := New(c).Apply(baseRequest(), statsWith(4000))

		if *out.Params.MaxTokens != 4000 {
			t.Errorf("max_tokens = %d, want 4000", *out.Params.MaxTokens)
		}
	})
}

// --- context pruning ---

func TestContextPruning(t *testing.T) {
	cfg := domain.RecommendedLevers()

	t.Run("off by default", func(t *testing.T) {
		// The only lever that changes what the model is asked, so it does not
		// ride along with the safe ones.
		if cfg.ContextPruning {
			t.Error("RecommendedLevers enables context pruning")
		}
		out, ops := New(cfg).Apply(baseRequest(), nil)
		if _, ok := find(ops, domain.LeverContextPrune); ok {
			t.Error("pruned without being enabled")
		}
		if len(out.Messages) != 3 {
			t.Errorf("messages = %d, want 3", len(out.Messages))
		}
	})

	t.Run("keeps the retention window", func(t *testing.T) {
		c := cfg
		c.ContextPruning = true
		c.KeepTurns = 2

		req := baseRequest()
		out, ops := New(c).Apply(req, nil)

		if _, ok := find(ops, domain.LeverContextPrune); !ok {
			t.Fatal("no prune optimization recorded")
		}
		if len(out.Messages) != 2 {
			t.Fatalf("messages = %d, want 2", len(out.Messages))
		}
		// The most recent turns are the ones kept. Compared by content rather
		// than DeepEqual, because breakpoint placement also runs here and sets
		// a flag on the parts.
		for i, want := range req.Messages[1:] {
			if out.Messages[i].Parts[0].Text != want.Parts[0].Text {
				t.Errorf("kept message %d is not the expected turn", i)
			}
		}
	})

	t.Run("system and tools are never pruned", func(t *testing.T) {
		c := cfg
		c.ContextPruning = true
		c.KeepTurns = 1

		out, _ := New(c).Apply(baseRequest(), nil)

		if len(out.System) == 0 || len(out.Tools) == 0 {
			t.Error("pruning removed instructions, not history")
		}
	})

	t.Run("does not orphan a tool result", func(t *testing.T) {
		// A tool result whose call was dropped is rejected by every provider.
		// Pruning that ignores pairing turns a working expensive request into
		// a failing one — a saving of zero and a broken customer.
		req := baseRequest()
		req.Messages = []domain.Message{
			msg(domain.RoleUser, "a"),
			{Role: domain.RoleAssistant, Parts: []domain.ContentPart{
				{Kind: domain.PartToolCall, ToolName: "search", Arguments: `{"q":"x"}`},
			}},
			{Role: domain.RoleTool, Parts: []domain.ContentPart{
				{Kind: domain.PartToolResult, ToolCallID: "1", Text: "result"},
			}},
			msg(domain.RoleUser, "b"),
		}

		c := cfg
		c.ContextPruning = true
		c.KeepTurns = 2 // would cut at index 2, orphaning the tool result

		out, _ := New(c).Apply(req, nil)

		if out.Messages[0].Role == domain.RoleTool || out.Messages[0].HasKind(domain.PartToolResult) {
			t.Error("pruning left an orphaned tool result at the head of the conversation")
		}
	})

	t.Run("never prunes everything", func(t *testing.T) {
		c := cfg
		c.ContextPruning = true
		c.KeepTurns = 1

		req := baseRequest()
		req.Messages = []domain.Message{
			{Role: domain.RoleTool, Parts: []domain.ContentPart{
				{Kind: domain.PartToolResult, Text: "r"},
			}},
			{Role: domain.RoleTool, Parts: []domain.ContentPart{
				{Kind: domain.PartToolResult, Text: "r"},
			}},
		}

		out, _ := New(c).Apply(req, nil)

		if len(out.Messages) == 0 {
			t.Error("pruned the entire conversation, including the request itself")
		}
	})

	t.Run("short conversations are left alone", func(t *testing.T) {
		c := cfg
		c.ContextPruning = true
		c.KeepTurns = 10

		out, ops := New(c).Apply(baseRequest(), nil)

		if _, ok := find(ops, domain.LeverContextPrune); ok {
			t.Error("pruned a conversation inside the retention window")
		}
		if len(out.Messages) != 3 {
			t.Errorf("messages = %d, want 3", len(out.Messages))
		}
	})
}

// --- estimate ---

func TestEstimateIsRecomputed(t *testing.T) {
	// Stale estimates would route against a request that no longer exists:
	// the input size feeds the context-window filter and the output ceiling
	// feeds both that filter and the cost estimate.
	cfg := domain.RecommendedLevers()
	cfg.ContextPruning = true
	cfg.KeepTurns = 1

	req := baseRequest()
	req.Estimate = domain.Estimate{InputTokens: 999999, MaxOutputTokens: 999999}

	out, _ := New(cfg).Apply(req, statsWith(400))

	if out.Estimate.InputTokens >= 999999 {
		t.Error("input estimate was not recomputed after pruning")
	}
	if out.Estimate.InputTokens != out.InputTokens() {
		t.Errorf("estimate %d disagrees with the request's own count %d",
			out.Estimate.InputTokens, out.InputTokens())
	}
	if out.Estimate.MaxOutputTokens != 600 { // 400 p95 x 1.5
		t.Errorf("MaxOutputTokens = %d, want 600", out.Estimate.MaxOutputTokens)
	}
	// Cost is estimated against expected output, not the ceiling: pricing every
	// request as its worst case would over-estimate cheap endpoints.
	if out.Estimate.ExpectedOutputTokens != 400 {
		t.Errorf("ExpectedOutputTokens = %d, want the p95 of 400",
			out.Estimate.ExpectedOutputTokens)
	}
}

func TestEstimateFallsBackWhenNoCeilingIsSet(t *testing.T) {
	// Some ceiling is required, or the context-fit check would treat the
	// request as producing no output and route it somewhere it cannot fit.
	out, _ := New(domain.LeverConfig{}).Apply(baseRequest(), nil)

	if out.Estimate.MaxOutputTokens != assumedMaxOutput {
		t.Errorf("MaxOutputTokens = %d, want the assumed %d",
			out.Estimate.MaxOutputTokens, assumedMaxOutput)
	}
}
