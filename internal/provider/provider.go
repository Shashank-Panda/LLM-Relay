// Package provider defines the contract every model provider implements.
//
// The interface is deliberately small. If adding a provider is a day's work,
// provider coverage grows; if it is a week's work, it does not — and a gateway
// whose value comes from choosing between endpoints cannot afford a provider
// layer that resists new endpoints.
//
// Everything generic lives above this package: retry policy, failover, circuit
// breaking, routing. The one exception is ClassifyError, which is part of the
// interface because only the adapter knows what a given vendor's error bodies
// mean. See architecture §4.
package provider

import (
	"context"
	"net/http"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// Adapter speaks one provider's wire protocol.
//
// Implementations must be safe for concurrent use: one Adapter serves every
// request to that provider, and it holds the pooled http.Client that makes
// connection reuse possible.
type Adapter interface {
	ID() domain.ProviderID

	// Chat performs a non-streaming completion.
	Chat(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, cred Credential) (*Response, error)

	// ChatStream begins a streaming completion. The returned Stream owns the
	// response body and must be closed by the caller on every path.
	ChatStream(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, cred Credential) (Stream, error)

	// ClassifyError maps a provider failure onto the generic taxonomy. Exactly
	// one of resp and err is expected to be meaningful.
	ClassifyError(resp *http.Response, err error) ErrorClass
}

// Stream yields chunks until io.EOF.
//
// Recv returning io.EOF is the normal termination and is not an error. Any
// other error terminates the stream too — there is no resuming a broken SSE
// connection, and pretending otherwise produces a response with a silent hole
// in the middle.
type Stream interface {
	Recv() (*Chunk, error)

	// Usage reports token counts. Valid only after Recv has returned io.EOF,
	// because providers emit usage in a final frame.
	Usage() *Usage

	// Close releases the underlying connection. Safe to call more than once.
	Close() error
}

// Role and content types are domain concepts; a Response is expressed in them
// so that nothing above this package needs to know which vendor answered.

// Response is a completed non-streaming answer.
type Response struct {
	// ID is the provider's own identifier for the completion, kept for support
	// escalations — when a customer disputes a response, the provider's ID is
	// what their support team can look up.
	ID string

	Model string

	// Parts is the assistant message: text, tool calls, reasoning blocks.
	Parts []domain.ContentPart

	FinishReason FinishReason
	Usage        Usage
}

// Chunk is one increment of a streaming answer.
//
// Deltas are already reassembled where reassembly is required: a chunk carrying
// tool-call arguments carries a syntactically meaningful fragment, and the
// stream guarantees that concatenating every fragment for a given index yields
// the complete JSON. Callers never see a fragment split mid-escape-sequence.
type Chunk struct {
	// Text is a content delta. Empty on chunks that carry only tool-call or
	// control information.
	Text string

	// Reasoning is a thinking-block delta, priced and surfaced separately.
	Reasoning string

	// ToolCall is set when this chunk advances a tool call.
	ToolCall *ToolCallDelta

	// FinishReason is set on the final content-bearing chunk, if the provider
	// reports one.
	FinishReason FinishReason
}

// ToolCallDelta advances one tool call in a stream.
//
// Index identifies which call, because providers may interleave several. Name
// and ID arrive once, on the first delta for that index; Arguments arrives in
// fragments that the caller concatenates. This mirrors the OpenAI streaming
// shape because it is the shape Relay re-emits, and translating into it inside
// each adapter keeps the re-emitter free of vendor conditionals.
type ToolCallDelta struct {
	Index int
	ID    string
	Name  string

	// Arguments is a fragment of raw JSON, not a complete document. It is
	// carried as text and never re-encoded: round-tripping through a map would
	// reorder keys, and some tools are sensitive to that.
	Arguments string
}

// FinishReason is why generation stopped, in OpenAI's vocabulary because that
// is what Relay's own API emits.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
)

// Usage is what the provider reported, not what Relay estimated.
type Usage struct {
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	ReasoningTokens   int

	// Estimated is true when the provider returned no usage block and these
	// numbers were derived instead.
	//
	// The flag exists so that estimated records can be excluded from precision
	// cost reporting rather than silently averaged in. An approximate number
	// that is indistinguishable from a measured one corrupts every aggregate it
	// enters. See architecture §8.
	Estimated bool
}

// Cost prices this usage against an endpoint.
//
// Cached input is priced at the cached rate where the endpoint declares one,
// and the cached tokens are deducted from the full-price input count — every
// provider reports cached tokens as a subset of input tokens, and charging both
// rates for the same token would overstate cost by the cached fraction.
func (u Usage) Cost(ep *domain.ModelEndpoint) domain.Money {
	if ep == nil {
		return 0
	}
	full := u.InputTokens
	var cached domain.Money
	if u.CachedInputTokens > 0 && ep.Pricing.CachedInput > 0 {
		full -= u.CachedInputTokens
		if full < 0 {
			full = 0
		}
		cached = ep.Pricing.CachedInput.Cost(u.CachedInputTokens)
	}
	return ep.Pricing.Input.Cost(full) +
		cached +
		ep.Pricing.Output.Cost(u.OutputTokens) +
		ep.Pricing.Reasoning.Cost(u.ReasoningTokens)
}

// ErrorClass decides what the executor does next. See architecture §6.
//
// Phase 1 has no retry loop, but every adapter classifies from the start:
// classification is knowledge about a vendor's error bodies, and recovering it
// later means re-reading three sets of API docs.
type ErrorClass string

const (
	// ClassRetrySame is transient and endpoint-specific: 429, 5xx, connection
	// reset, read timeout. Back off and try the same endpoint again.
	ClassRetrySame ErrorClass = "RetrySame"

	// ClassRetryOther means this endpoint is unhealthy but the request is fine.
	// Move to the next candidate.
	ClassRetryOther ErrorClass = "RetryOther"

	// ClassReroute means the constraint set used for routing was wrong —
	// context_length_exceeded, a removed model, an unsupported capability.
	// Neither transient nor permanent: re-filter with the corrected constraint.
	ClassReroute ErrorClass = "Reroute"

	// ClassTerminal is a request that will fail identically everywhere: 400,
	// 401, 403, content filter, invalid tool schema.
	//
	// The most important class. Retrying a malformed request across three
	// providers produces three bills and one guaranteed failure.
	ClassTerminal ErrorClass = "Terminal"

	// ClassCancelled is the client going away or a deadline expiring. Never
	// retried — nobody is waiting for the answer.
	ClassCancelled ErrorClass = "Cancelled"
)

// Retryable reports whether this class permits another attempt anywhere.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ClassRetrySame, ClassRetryOther, ClassReroute:
		return true
	}
	return false
}
