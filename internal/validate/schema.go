package validate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// checkSchema validates a decoded document against a bounded subset of JSON
// Schema.
//
// A subset, and stated as one rather than presented as full validation. Full
// JSON Schema is a large specification with `$ref`, `allOf`/`anyOf`/`oneOf`,
// conditional application, and a resolution model — implementing it here would
// be a project, and importing it would be a dependency decision that belongs in
// its own ADR rather than smuggled in under a validity check.
//
// What matters is which direction the subset errs in. Every construct this does
// not understand is *skipped*, so an unhandled schema produces "valid" and no
// escalation. That means the checker can miss a violation, which costs nothing —
// the response is returned exactly as it would have been. It cannot invent one,
// which would spend a second provider call on output that was fine. ADR-0009
// asks for checks that are cheap and objective; the subset below is both, and
// the parts it declines to judge stay out of the way.
//
// Covered: type, required, properties (recursively), items, enum, and
// additionalProperties: false. Everything else is ignored.
func checkSchema(raw string, doc any) Result {
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		// The schema the *caller* supplied does not parse. That is their bug and
		// not the model's, and escalating to a dearer model would produce the
		// same output against the same broken schema.
		return valid()
	}
	if v := violation(schema, doc, "$"); v != "" {
		return invalid(ReasonSchemaViolation, "%s", v)
	}
	return valid()
}

// violation returns a description of the first breach found, or "".
func violation(schema map[string]any, doc any, path string) string {
	if len(schema) == 0 {
		return ""
	}

	// A schema combining subschemas is outside the subset. Skipping the whole
	// node rather than checking the parts is deliberate: `anyOf` means the
	// document need only satisfy one branch, and a checker that tested the
	// wrong branch would report a violation that is not one.
	for _, k := range []string{"anyOf", "oneOf", "allOf", "not", "$ref", "if"} {
		if _, ok := schema[k]; ok {
			return ""
		}
	}

	if v := typeViolation(schema, doc, path); v != "" {
		return v
	}
	if v := enumViolation(schema, doc, path); v != "" {
		return v
	}

	switch d := doc.(type) {
	case map[string]any:
		return objectViolation(schema, d, path)
	case []any:
		return arrayViolation(schema, d, path)
	}
	return ""
}

func typeViolation(schema map[string]any, doc any, path string) string {
	want, ok := schema["type"].(string)
	if !ok {
		// Absent, or a type *union* expressed as an array. Unions are within
		// the spec and outside this subset.
		return ""
	}
	if matchesType(want, doc) {
		return ""
	}
	return fmt.Sprintf("%s: expected type %s, got %s", path, want, jsonTypeOf(doc))
}

// matchesType compares against JSON's type model, not Go's.
//
// The integer case is the one that bites. JSON has no integer type and
// encoding/json decodes every number to float64, so a document containing 3 is
// indistinguishable from one containing 3.0 by the time it reaches here. A
// checker that rejected `3` for `type: integer` would flag correct output on
// every schema that uses integers.
func matchesType(want string, doc any) bool {
	switch want {
	case "object":
		_, ok := doc.(map[string]any)
		return ok
	case "array":
		_, ok := doc.([]any)
		return ok
	case "string":
		_, ok := doc.(string)
		return ok
	case "number", "integer":
		f, ok := doc.(float64)
		if !ok {
			return false
		}
		if want == "integer" {
			return f == float64(int64(f))
		}
		return true
	case "boolean":
		_, ok := doc.(bool)
		return ok
	case "null":
		return doc == nil
	}
	// An unrecognised type name is the schema's problem, not the response's.
	return true
}

func enumViolation(schema map[string]any, doc any, path string) string {
	values, ok := schema["enum"].([]any)
	if !ok || len(values) == 0 {
		return ""
	}
	for _, v := range values {
		if equalJSON(v, doc) {
			return ""
		}
	}
	return fmt.Sprintf("%s: %v is not one of the enumerated values", path, doc)
}

func objectViolation(schema map[string]any, doc map[string]any, path string) string {
	props, _ := schema["properties"].(map[string]any)

	if required, ok := schema["required"].([]any); ok {
		for _, r := range required {
			name, ok := r.(string)
			if !ok {
				continue
			}
			if _, present := doc[name]; !present {
				return fmt.Sprintf("%s: required property %q is missing", path, name)
			}
		}
	}

	// additionalProperties: false is the one form checked. The schema form —
	// where it constrains the *shape* of extra properties — is outside the
	// subset.
	if extra, ok := schema["additionalProperties"].(bool); ok && !extra && props != nil {
		for name := range doc {
			if _, declared := props[name]; !declared {
				return fmt.Sprintf("%s: property %q is not permitted", path, name)
			}
		}
	}

	for name, sub := range props {
		value, present := doc[name]
		if !present {
			continue
		}
		subSchema, ok := sub.(map[string]any)
		if !ok {
			continue
		}
		if v := violation(subSchema, value, path+"."+name); v != "" {
			return v
		}
	}
	return ""
}

func arrayViolation(schema map[string]any, doc []any, path string) string {
	items, ok := schema["items"].(map[string]any)
	if !ok {
		// Tuple validation (items as an array) is outside the subset.
		return ""
	}
	for i, v := range doc {
		if msg := violation(items, v, fmt.Sprintf("%s[%d]", path, i)); msg != "" {
			return msg
		}
	}
	return ""
}

// equalJSON compares two decoded values structurally.
//
// Re-encoding rather than reflect.DeepEqual, because enum values arrive from
// one decode and the document from another, and numeric values that are equal
// as JSON can differ as Go values across those paths.
func equalJSON(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

func jsonTypeOf(doc any) string {
	switch v := doc.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if v == float64(int64(v)) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return strings.ToLower(fmt.Sprintf("%T", doc))
}
