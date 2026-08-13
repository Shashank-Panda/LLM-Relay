package domain

import "slices"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type PartKind string

const (
	PartText       PartKind = "text"
	PartImage      PartKind = "image"
	PartToolCall   PartKind = "tool_call"
	PartToolResult PartKind = "tool_result"
	PartReasoning  PartKind = "reasoning"
)

// ContentPart is one piece of a message. Modelling content as parts rather than
// a string is what makes multimodal input, tool calls, and reasoning blocks
// expressible in a single provider-neutral shape.
type ContentPart struct {
	Kind PartKind

	Text string

	// Image
	MediaType string
	URL       string
	Data      string // base64, when the caller inlined it

	// Tool call and result
	ToolCallID string
	ToolName   string
	Arguments  string // raw JSON, kept as text: re-encoding would reorder keys

	// CacheBreakpoint marks this part as the end of a cacheable prefix.
	//
	// Set by the optimizer, which decides *where* a breakpoint belongs — a
	// provider-neutral question about the request's stable prefix. How a
	// breakpoint is spelled on the wire is provider-specific and stays in the
	// adapter.
	CacheBreakpoint bool
}

// ApproxTokens estimates the token cost of this part.
func (p ContentPart) ApproxTokens() int {
	switch p.Kind {
	case PartImage:
		// Image tokenization is provider- and resolution-specific and cannot be
		// computed from the URL. A flat estimate keeps the number honest about
		// being an estimate rather than pretending to a precision it lacks.
		return approxImageTokens
	case PartToolCall, PartToolResult:
		return ApproxTokens(p.Text) + ApproxTokens(p.Arguments) + ApproxTokens(p.ToolName)
	default:
		return ApproxTokens(p.Text)
	}
}

const approxImageTokens = 1_000

type Message struct {
	Role  Role
	Name  string
	Parts []ContentPart
}

func (m Message) ApproxTokens() int {
	// Per-message framing overhead: role, delimiters, and the small fixed cost
	// every provider adds around a message.
	n := 4
	for _, p := range m.Parts {
		n += p.ApproxTokens()
	}
	return n
}

// HasKind reports whether any part is of the given kind.
func (m Message) HasKind(k PartKind) bool {
	return slices.ContainsFunc(m.Parts, func(p ContentPart) bool { return p.Kind == k })
}

type ToolDef struct {
	Name        string
	Description string
	Schema      string // raw JSON Schema

	CacheBreakpoint bool
}

func (t ToolDef) ApproxTokens() int {
	return ApproxTokens(t.Name) + ApproxTokens(t.Description) + ApproxTokens(t.Schema)
}

type ResponseFormatType string

const (
	FormatText       ResponseFormatType = "text"
	FormatJSONObject ResponseFormatType = "json_object"
	FormatJSONSchema ResponseFormatType = "json_schema"
)

type ResponseFormat struct {
	Type   ResponseFormatType
	Schema string
}

// ReasoningEffort is an ordered budget for models that expose one.
type ReasoningEffort string

const (
	EffortMinimal ReasoningEffort = "minimal"
	EffortLow     ReasoningEffort = "low"
	EffortMedium  ReasoningEffort = "medium"
	EffortHigh    ReasoningEffort = "high"
)

var effortOrder = []ReasoningEffort{EffortMinimal, EffortLow, EffortMedium, EffortHigh}

// Level returns the effort's rank, or -1 if unrecognised.
func (e ReasoningEffort) Level() int { return slices.Index(effortOrder, e) }

func (e ReasoningEffort) Valid() bool { return e.Level() >= 0 }

// SamplingParams carries generation settings.
//
// The pointer fields are the load-bearing design choice in this file. The
// optimizer's first rule is that it never overrides an explicit caller value —
// and with a plain int there is no way to tell `max_tokens: 0` from a caller
// who said nothing at all. A nil pointer means unset; a non-nil pointer is a
// decision the caller made and the optimizer must leave alone.
type SamplingParams struct {
	Temperature     *float64
	TopP            *float64
	MaxTokens       *int
	Seed            *int64
	ReasoningEffort *ReasoningEffort
	Stop            []string

	// EffortFromOptimizer marks ReasoningEffort as a Relay-applied default
	// rather than caller intent.
	//
	// The distinction is load-bearing. A caller-set effort is a hard capability
	// requirement — send it to a model without reasoning support and you get a
	// 400 — so it must narrow the candidate set. A Relay-applied default must
	// not, or enabling the lever would silently eliminate every non-reasoning
	// endpoint from every route. Adapters drop an optimizer-applied effort for
	// endpoints that do not support one; a caller-set effort they must honour.
	EffortFromOptimizer bool
}

// Helpers for constructing params in normalization and in tests.
func Ptr[T any](v T) *T { return &v }

// ApproxTokens estimates a token count from text.
//
// Roughly four bytes per token, which is a reasonable average for English prose
// and code and is wrong for everything else. It is used for two purposes that
// tolerate it: deciding whether a prefix is long enough to be worth caching,
// and pre-routing cost estimation. Anything billed uses provider-reported
// actuals instead — see architecture §8.
func ApproxTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}
