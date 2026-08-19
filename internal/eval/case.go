// Package eval is the offline harness that turns asserted quality scores into
// measured ones.
//
// The catalog's `quality:` block is an operator assertion. ADR-0009 is explicit
// that this is acceptable for a self-hosted gateway and not acceptable as the
// sole basis of a commercial claim that output will not degrade — so the scores
// have to come from somewhere better, and this is the first of the three
// replacements that ADR names: offline evals per task type, re-run whenever the
// catalog changes.
//
// Two deliberate constraints on what an eval may assert:
//
//   - **Objective checks only.** No model judges another model here. LLM-as-judge
//     is rejected in the request path because it adds an inference to every
//     request, and it is avoided offline too for a different reason: a score
//     produced by a judge is a claim about the judge as much as the candidate,
//     and it cannot be reproduced by a customer who wants to check the number.
//     Everything below is a check somebody can run by hand and get the same
//     answer.
//   - **Per task type.** A single "quality" number is a poor predictor for any
//     specific workload, which is why the catalog scores by dimension. The suite
//     is grouped the same way so the output drops straight into it.
//
// Offline is not a limitation to be worked around. It runs against real
// providers at real cost, on a fixed set, whenever the catalog changes — not on
// customer traffic and never on the hot path.
package eval

import (
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// Case is one graded request.
type Case struct {
	// ID names the case in the report. Stable across runs, because a score that
	// moved is only interesting next to which cases moved with it.
	ID string `yaml:"id"`

	// Dimension is the catalog quality dimension this case scores: "coding",
	// "reasoning", "long_context". It is the key the result is written under.
	Dimension string `yaml:"dimension"`

	// Weight lets one case count for more than another within a dimension.
	// Zero means one.
	Weight float64 `yaml:"weight"`

	System string `yaml:"system"`
	Prompt string `yaml:"prompt"`

	// Tools, when present, are offered to the model and make the tool-call
	// expectations meaningful.
	Tools []ToolSpec `yaml:"tools"`

	// ResponseFormat is "text", "json_object", or "json_schema".
	ResponseFormat string `yaml:"response_format"`
	Schema         string `yaml:"schema"`

	MaxTokens int `yaml:"max_tokens"`

	// Expect is what a correct answer looks like. Every listed expectation must
	// hold; a case with none is valid and checks only that the response was
	// well-formed, which is a real thing to measure.
	Expect Expect `yaml:"expect"`
}

// ToolSpec is a tool offered to the model during an eval.
type ToolSpec struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Schema      string `yaml:"schema"`
}

// Expect is the objective grading criteria for one case.
//
// Every field is a check a person could run by hand against the response and get
// the same verdict. Nothing here is a judgement call, which is what makes a
// score derived from them auditable by the customer it is quoted to.
type Expect struct {
	// Contains requires every listed substring, case-insensitively. The usual
	// shape for "did it get the answer right" without demanding exact phrasing.
	Contains []string `yaml:"contains"`

	// NotContains fails the case if any listed substring appears. Useful for
	// refusal phrasing and for the wrong answer to a question with a
	// near-miss.
	NotContains []string `yaml:"not_contains"`

	// Regex requires every listed pattern to match.
	Regex []string `yaml:"regex"`

	// CallsTool requires a tool call to this name.
	CallsTool string `yaml:"calls_tool"`

	// ToolArgs requires these keys to be present in the tool call's arguments,
	// with these values when a value is given. Values compare as strings after
	// JSON decoding, so 3 and "3" both match "3" — the eval is grading whether
	// the model extracted the right argument, not how a provider encoded it.
	ToolArgs map[string]string `yaml:"tool_args"`

	// JSONPath requires these dotted paths to be present in a JSON response,
	// with these values when a value is given.
	JSONPath map[string]string `yaml:"json_path"`

	// ValidOnly asserts nothing beyond the response being well-formed: not
	// empty, parseable in the declared format, tool calls declared and parsing.
	// Set implicitly whenever nothing else is specified.
	ValidOnly bool `yaml:"valid_only"`
}

func (e Expect) isEmpty() bool {
	return len(e.Contains) == 0 && len(e.NotContains) == 0 && len(e.Regex) == 0 &&
		e.CallsTool == "" && len(e.ToolArgs) == 0 && len(e.JSONPath) == 0
}

// Suite is a set of cases plus how to run them.
type Suite struct {
	Name  string `yaml:"name"`
	Cases []Case `yaml:"cases"`

	// Repeats is how many times each case runs. Above one it measures
	// consistency as well as correctness, which matters: a model that answers
	// correctly two times in three is a different proposition from one that
	// always does, and a single run cannot tell them apart.
	Repeats int `yaml:"repeats"`

	// Temperature is applied to every case. Zero by default, because an eval
	// measuring a model rather than its sampling should remove what variance it
	// can.
	Temperature float64 `yaml:"temperature"`
}

// LoadFile reads a suite from disk.
func LoadFile(path string) (*Suite, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("eval: %w", err)
	}
	defer f.Close()

	s, err := Load(f)
	if err != nil {
		return nil, fmt.Errorf("eval %s: %w", path, err)
	}
	return s, nil
}

// Load reads a suite from a reader and validates it.
//
// Strict decoding, like every other config file here: a misspelled expectation
// that is silently ignored produces a case that passes unconditionally, and a
// quality score built partly from unconditional passes is worse than no score.
func Load(r io.Reader) (*Suite, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var s Suite
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if s.Repeats <= 0 {
		s.Repeats = 1
	}

	seen := map[string]bool{}
	var problems []string
	for i := range s.Cases {
		c := &s.Cases[i]
		where := fmt.Sprintf("cases[%d]", i)
		if c.ID != "" {
			where = fmt.Sprintf("cases[%d] %q", i, c.ID)
		}

		switch {
		case c.ID == "":
			problems = append(problems, where+": id is required")
		case seen[c.ID]:
			problems = append(problems, where+": duplicate id")
		default:
			seen[c.ID] = true
		}
		if c.Dimension == "" {
			problems = append(problems, where+": dimension is required; it is the catalog key the score is written under")
		}
		if c.Prompt == "" {
			problems = append(problems, where+": prompt is required")
		}
		if c.Weight == 0 {
			c.Weight = 1
		}
		if c.Weight < 0 {
			problems = append(problems, where+": weight must not be negative")
		}
		if c.Expect.CallsTool != "" && len(c.Tools) == 0 {
			problems = append(problems, where+": expects a tool call but offers no tools")
		}
		if c.Expect.isEmpty() {
			c.Expect.ValidOnly = true
		}
		if format := c.ResponseFormat; format != "" &&
			format != "text" && format != "json_object" && format != "json_schema" {
			problems = append(problems, where+": response_format must be text, json_object, or json_schema")
		}
		if c.ResponseFormat == "json_schema" && c.Schema == "" {
			problems = append(problems, where+": response_format json_schema needs a schema")
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("%d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return &s, nil
}

// Request renders a case as a normalized request.
func (c Case) Request(temperature float64) *domain.NormalizedRequest {
	req := &domain.NormalizedRequest{
		Messages: []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.ContentPart{{Kind: domain.PartText, Text: c.Prompt}},
		}},
	}
	if c.System != "" {
		req.System = []domain.ContentPart{{Kind: domain.PartText, Text: c.System}}
	}
	for _, t := range c.Tools {
		req.Tools = append(req.Tools, domain.ToolDef{
			Name: t.Name, Description: t.Description, Schema: t.Schema,
		})
	}
	switch c.ResponseFormat {
	case "json_object":
		req.ResponseFormat = domain.ResponseFormat{Type: domain.FormatJSONObject}
	case "json_schema":
		req.ResponseFormat = domain.ResponseFormat{Type: domain.FormatJSONSchema, Schema: c.Schema}
	}
	if c.MaxTokens > 0 {
		req.Params.MaxTokens = domain.Ptr(c.MaxTokens)
	}
	req.Params.Temperature = domain.Ptr(temperature)

	req.Estimate = domain.Estimate{
		InputTokens:          req.InputTokens(),
		MaxOutputTokens:      max(c.MaxTokens, 1024),
		ExpectedOutputTokens: max(c.MaxTokens, 1024),
	}
	return req
}
