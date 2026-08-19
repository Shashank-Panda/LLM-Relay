// Package validate decides whether a response is invalid, cheaply and
// objectively.
//
// It is the trigger for cascade escalation (ADR-0009): when a downgraded
// endpoint returns something that fails one of these checks, the executor
// retries once on the baseline. That makes an over-optimistic quality score in
// the catalog self-correcting instead of silently harmful.
//
// The limit is the important part and must not be oversold. These checks detect
// *invalid* output, never *worse* output. A cheaper model that returns a
// well-formed, schema-valid, subtly inferior answer passes every one of them.
// Escalation is a floor on correctness; `Policy.QualityFloor` is the separate,
// upstream mechanism that is a floor on quality.
//
// One rule governs every check here: **when in doubt, valid.** The two failure
// directions are not symmetric. A missed violation costs nothing — the response
// is returned as it would have been anyway. A false alarm spends a second
// provider call on a response that was fine, which is money, latency, and a
// negative saving in the ledger. So every check below either proves a violation
// or declines to judge.
package validate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// Reason names why a response was rejected. A closed set: it becomes a
// Prometheus label and a ledger field.
type Reason string

const (
	ReasonValid Reason = ""

	// ReasonEmpty is a response with no content of any kind. Note that a pure
	// tool call is not empty — it is the answer, in the shape the caller asked
	// for.
	ReasonEmpty Reason = "empty_completion"

	// ReasonInvalidJSON is a response_format: json_object whose text does not
	// parse.
	ReasonInvalidJSON Reason = "invalid_json"

	// ReasonSchemaViolation is a response_format: json_schema whose output
	// breaks the declared schema in one of the ways this package checks.
	ReasonSchemaViolation Reason = "schema_violation"

	// ReasonInvalidToolCall is a tool call whose arguments are not valid JSON.
	// The caller is about to unmarshal them, so this fails at the call site
	// rather than here if it is not caught.
	ReasonInvalidToolCall Reason = "invalid_tool_call"

	// ReasonUndeclaredTool is a call to a tool the request never offered. It is
	// the clearest possible signal that a model did not follow instructions.
	ReasonUndeclaredTool Reason = "undeclared_tool"

	// ReasonRefusal is a safety stop on a request the baseline would likely
	// have accepted.
	ReasonRefusal Reason = "refusal"
)

// Result is one validity judgement.
type Result struct {
	Valid  bool
	Reason Reason
	Detail string
}

func valid() Result { return Result{Valid: true} }

func invalid(r Reason, format string, args ...any) Result {
	return Result{Reason: r, Detail: fmt.Sprintf(format, args...)}
}

// Check evaluates a completed response against what the request asked for.
//
// Order matters only in which reason gets reported first, and the order here is
// cheapest and most certain first: presence, then declared output contract, then
// tool contract, then the one heuristic.
func Check(req *domain.NormalizedRequest, resp *provider.Response) Result {
	if req == nil || resp == nil {
		return valid()
	}

	if r := checkNonEmpty(resp); !r.Valid {
		return r
	}
	if r := checkResponseFormat(req, resp); !r.Valid {
		return r
	}
	if r := checkToolCalls(req, resp); !r.Valid {
		return r
	}
	return checkRefusal(resp)
}

// checkNonEmpty rejects a response that said nothing at all.
//
// "Nothing" means no text, no tool call, and no reasoning. A pure tool-call
// response carries no text by design and is the correct answer to a
// tool-calling request; treating it as empty would escalate every successful
// function call a cheap model makes.
func checkNonEmpty(resp *provider.Response) Result {
	for _, p := range resp.Parts {
		switch p.Kind {
		case domain.PartToolCall:
			return valid()
		case domain.PartText, domain.PartReasoning:
			if strings.TrimSpace(p.Text) != "" {
				return valid()
			}
		default:
			// An image or anything else this build does not know about is
			// content. Declining to judge is the conservative direction.
			return valid()
		}
	}
	return invalid(ReasonEmpty, "response contained no text, tool call, or reasoning")
}

func checkResponseFormat(req *domain.NormalizedRequest, resp *provider.Response) Result {
	switch req.ResponseFormat.Type {
	case domain.FormatJSONObject, domain.FormatJSONSchema:
	default:
		return valid()
	}

	text := textOf(resp)
	if strings.TrimSpace(text) == "" {
		// A tool call satisfies a JSON-mode request in some providers' shapes;
		// checkNonEmpty already established there is content of some kind.
		return valid()
	}

	var doc any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return invalid(ReasonInvalidJSON,
			"response_format is %s but the body does not parse: %v",
			req.ResponseFormat.Type, err)
	}

	if req.ResponseFormat.Type != domain.FormatJSONSchema || req.ResponseFormat.Schema == "" {
		return valid()
	}
	return checkSchema(req.ResponseFormat.Schema, doc)
}

func checkToolCalls(req *domain.NormalizedRequest, resp *provider.Response) Result {
	declared := make(map[string]bool, len(req.Tools))
	for _, t := range req.Tools {
		declared[t.Name] = true
	}

	for _, p := range resp.Parts {
		if p.Kind != domain.PartToolCall {
			continue
		}
		if len(declared) > 0 && p.ToolName != "" && !declared[p.ToolName] {
			return invalid(ReasonUndeclaredTool,
				"called %q, which the request did not declare", p.ToolName)
		}
		// Empty arguments are how a zero-parameter tool is called, and are not
		// a violation. Anything else must parse: the caller is about to
		// unmarshal it, and failing here is cheaper than failing there.
		if strings.TrimSpace(p.Arguments) == "" {
			continue
		}
		var args any
		if err := json.Unmarshal([]byte(p.Arguments), &args); err != nil {
			return invalid(ReasonInvalidToolCall,
				"arguments for %q do not parse: %v", p.ToolName, err)
		}
	}
	return valid()
}

// checkRefusal detects a safety stop.
//
// The finish reason only, deliberately. The obvious extra check is to look for
// refusal phrasing in the text — "I can't help with that" — and it is a trap:
// it is locale-specific, it is defeated by paraphrase, and it fires on the
// perfectly good answer to "what should I say when I have to decline a
// request". A false alarm here spends a second provider call on a correct
// response, so a heuristic with a real false-positive rate is worse than no
// heuristic at all. `content_filter` is the provider stating it, which is a
// fact rather than a guess.
func checkRefusal(resp *provider.Response) Result {
	if resp.FinishReason == provider.FinishContentFilter {
		return invalid(ReasonRefusal, "provider stopped the response with content_filter")
	}
	return valid()
}

func textOf(resp *provider.Response) string {
	var b strings.Builder
	for _, p := range resp.Parts {
		if p.Kind == domain.PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
