package domain

import "slices"

// NormalizedRequest is the provider-neutral form of an inbound request.
// Everything downstream operates on this; nothing downstream reads the raw
// HTTP body.
//
// Note what the router gets instead: RoutingView strips the content away. The
// optimizer needs to see messages — it cannot place a cache breakpoint without
// knowing where the stable prefix ends — while the router must be explainable
// from structured fields alone. A routing decision that depended on message
// text could not be reproduced from a Decision, and every explanation Relay
// gives a customer would be a reconstruction rather than a record.
type NormalizedRequest struct {
	ID     string
	Tenant string

	RouteName string
	Baseline  Baseline

	System   []ContentPart
	Messages []Message
	Tools    []ToolDef

	ToolChoice     string
	ResponseFormat ResponseFormat
	Params         SamplingParams
	Stream         bool

	SessionKey       string
	PreviousEndpoint string

	// NoCache bypasses the response cache in both directions for this request.
	//
	// Both, deliberately. A bypass that still wrote to the cache would let a
	// caller who asked for a fresh answer decide what every subsequent caller
	// gets served, which is the opposite of what they asked for.
	NoCache bool

	Estimate Estimate
	Metadata map[string]string
}

// Clone returns a deep copy.
//
// The optimizer returns a modified request rather than mutating in place, so
// the original survives for retries, failover, and the baseline comparison. A
// shallow copy would not be enough: setting a cache breakpoint writes into a
// ContentPart inside a shared slice.
func (r *NormalizedRequest) Clone() *NormalizedRequest {
	if r == nil {
		return nil
	}
	out := *r

	out.System = slices.Clone(r.System)
	out.Tools = slices.Clone(r.Tools)

	out.Messages = make([]Message, len(r.Messages))
	for i, m := range r.Messages {
		m.Parts = slices.Clone(m.Parts)
		out.Messages[i] = m
	}

	out.Params.Stop = slices.Clone(r.Params.Stop)
	out.Params.Temperature = clonePtr(r.Params.Temperature)
	out.Params.TopP = clonePtr(r.Params.TopP)
	out.Params.MaxTokens = clonePtr(r.Params.MaxTokens)
	out.Params.Seed = clonePtr(r.Params.Seed)
	out.Params.ReasoningEffort = clonePtr(r.Params.ReasoningEffort)

	if r.Metadata != nil {
		out.Metadata = make(map[string]string, len(r.Metadata))
		for k, v := range r.Metadata {
			out.Metadata[k] = v
		}
	}
	return &out
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// InputTokens approximates the request's input size.
func (r *NormalizedRequest) InputTokens() int {
	n := 0
	for _, p := range r.System {
		n += p.ApproxTokens()
	}
	for _, t := range r.Tools {
		n += t.ApproxTokens()
	}
	for _, m := range r.Messages {
		n += m.ApproxTokens()
	}
	return n
}

// Needs derives the capability requirements from the request itself.
//
// Derived rather than declared, because a hand-set Need is a field someone will
// eventually forget to update — and the consequence is routing to an endpoint
// that cannot serve the request at all.
func (r *NormalizedRequest) Needs() Need {
	n := Need{
		Tools:      len(r.Tools) > 0,
		JSONSchema: r.ResponseFormat.Type == FormatJSONSchema,
		Streaming:  r.Stream,
		// Only a caller-set effort is a requirement. A default the optimizer
		// filled in must not narrow the candidate set — see EffortFromOptimizer.
		Reasoning: r.Params.ReasoningEffort != nil && !r.Params.EffortFromOptimizer,
	}
	for _, m := range r.Messages {
		if m.HasKind(PartImage) {
			n.Vision = true
			break
		}
	}
	if !n.Vision {
		for _, p := range r.System {
			if p.Kind == PartImage {
				n.Vision = true
				break
			}
		}
	}
	return n
}

// RoutingView projects the request down to what the router is allowed to see.
func (r *NormalizedRequest) RoutingView() *Request {
	return &Request{
		ID:               r.ID,
		Tenant:           r.Tenant,
		RouteName:        r.RouteName,
		Baseline:         r.Baseline,
		Need:             r.Needs(),
		Estimate:         r.Estimate,
		SessionKey:       r.SessionKey,
		PreviousEndpoint: r.PreviousEndpoint,
	}
}
