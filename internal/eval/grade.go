package eval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/validate"
)

// Grade is one case's verdict on one response.
type Grade struct {
	Passed bool
	// Failures lists every expectation that did not hold, not just the first.
	// A model that missed three things is a different signal from one that
	// missed one, and re-running to discover the second failure costs another
	// provider call.
	Failures []string
}

// grade checks a response against a case's expectations.
//
// Validity comes first and is applied to every case regardless of what else was
// asked for. It uses the same checker the request path uses for cascade
// escalation, deliberately: if an endpoint's output would be escalated in
// production, that is a failure here too, and having two definitions of
// "invalid" would let a model score well on evals while escalating constantly
// in production.
func grade(c Case, req *domain.NormalizedRequest, resp *provider.Response) Grade {
	var g Grade

	if v := validate.Check(req, resp); !v.Valid {
		g.Failures = append(g.Failures, fmt.Sprintf("invalid response (%s): %s", v.Reason, v.Detail))
		return g
	}
	if c.Expect.ValidOnly {
		g.Passed = true
		return g
	}

	text := responseText(resp)
	lower := strings.ToLower(text)

	for _, want := range c.Expect.Contains {
		if !strings.Contains(lower, strings.ToLower(want)) {
			g.Failures = append(g.Failures, fmt.Sprintf("missing %q", want))
		}
	}
	for _, bad := range c.Expect.NotContains {
		if strings.Contains(lower, strings.ToLower(bad)) {
			g.Failures = append(g.Failures, fmt.Sprintf("contains %q", bad))
		}
	}
	for _, pattern := range c.Expect.Regex {
		re, err := regexp.Compile(pattern)
		if err != nil {
			// The suite's bug, not the model's. Failing the case would penalise
			// an endpoint for the eval author's typo, so this is reported and
			// the case is not held against it.
			g.Failures = append(g.Failures, fmt.Sprintf("suite error: bad regex %q: %v", pattern, err))
			continue
		}
		if !re.MatchString(text) {
			g.Failures = append(g.Failures, fmt.Sprintf("no match for /%s/", pattern))
		}
	}

	g.Failures = append(g.Failures, gradeToolCall(c, resp)...)
	g.Failures = append(g.Failures, gradeJSONPaths(c, text)...)

	g.Passed = len(g.Failures) == 0
	return g
}

func gradeToolCall(c Case, resp *provider.Response) []string {
	if c.Expect.CallsTool == "" && len(c.Expect.ToolArgs) == 0 {
		return nil
	}

	var call *domain.ContentPart
	for i, p := range resp.Parts {
		if p.Kind != domain.PartToolCall {
			continue
		}
		if c.Expect.CallsTool == "" || p.ToolName == c.Expect.CallsTool {
			call = &resp.Parts[i]
			break
		}
	}
	if call == nil {
		return []string{fmt.Sprintf("did not call %q", c.Expect.CallsTool)}
	}
	if len(c.Expect.ToolArgs) == 0 {
		return nil
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return []string{fmt.Sprintf("tool arguments do not parse: %v", err)}
	}

	var out []string
	for _, key := range sortedKeys(c.Expect.ToolArgs) {
		want := c.Expect.ToolArgs[key]
		got, present := args[key]
		if !present {
			out = append(out, fmt.Sprintf("tool argument %q missing", key))
			continue
		}
		if want == "" {
			continue // presence was the whole requirement
		}
		if !scalarEqual(got, want) {
			out = append(out, fmt.Sprintf("tool argument %q = %v, want %q", key, got, want))
		}
	}
	return out
}

func gradeJSONPaths(c Case, text string) []string {
	if len(c.Expect.JSONPath) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return []string{fmt.Sprintf("response is not JSON: %v", err)}
	}

	var out []string
	for _, path := range sortedKeys(c.Expect.JSONPath) {
		want := c.Expect.JSONPath[path]
		got, ok := lookup(doc, path)
		if !ok {
			out = append(out, fmt.Sprintf("%s is missing", path))
			continue
		}
		if want == "" {
			continue
		}
		if !scalarEqual(got, want) {
			out = append(out, fmt.Sprintf("%s = %v, want %q", path, got, want))
		}
	}
	return out
}

// lookup walks a dotted path, with [n] for array indices.
//
// A deliberately small subset of JSONPath: dotted keys and numeric indices,
// nothing else. An eval expectation should be readable at a glance by whoever
// has to decide whether a failing case is the model's fault or the suite's, and
// a query language defeats that.
func lookup(doc any, path string) (any, bool) {
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		name, indices := parseSegment(seg)
		if name != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			cur, ok = m[name]
			if !ok {
				return nil, false
			}
		}
		for _, i := range indices {
			arr, ok := cur.([]any)
			if !ok || i < 0 || i >= len(arr) {
				return nil, false
			}
			cur = arr[i]
		}
	}
	return cur, true
}

func parseSegment(seg string) (name string, indices []int) {
	for {
		open := strings.Index(seg, "[")
		if open < 0 {
			return name + seg, indices
		}
		close := strings.Index(seg[open:], "]")
		if close < 0 {
			return name + seg, indices
		}
		close += open

		name += seg[:open]
		n, err := strconv.Atoi(seg[open+1 : close])
		if err != nil {
			return name, indices
		}
		indices = append(indices, n)
		seg = seg[close+1:]
	}
}

// scalarEqual compares a decoded JSON value to an expectation written as a
// string in a YAML file.
//
// Everything in an expectation is a string, because YAML would otherwise decide
// whether `3` is a number and `true` is a boolean, and the suite author would be
// debugging type coercion instead of writing evals. Comparing the rendered form
// makes 3, 3.0, and "3" all match "3" — which is right, because the eval is
// grading whether the model produced the right answer rather than how a provider
// serialized it.
func scalarEqual(got any, want string) bool {
	switch v := got.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(want))
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10) == strings.TrimSpace(want)
		}
		return strconv.FormatFloat(v, 'g', -1, 64) == strings.TrimSpace(want)
	case bool:
		return strconv.FormatBool(v) == strings.ToLower(strings.TrimSpace(want))
	case nil:
		return strings.EqualFold(want, "null")
	}
	buf, err := json.Marshal(got)
	return err == nil && string(buf) == want
}

func responseText(resp *provider.Response) string {
	var b strings.Builder
	for _, p := range resp.Parts {
		if p.Kind == domain.PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string { return domain.SortedKeys(m) }
