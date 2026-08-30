package catalog

import (
	"strings"
	"testing"
)

func TestExpandEnv(t *testing.T) {
	t.Setenv("RELAY_TEST_HOST", "http://ollama:11434")
	t.Setenv("RELAY_TEST_EMPTY", "")

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"no expansion", "http://localhost:11434", "http://localhost:11434"},
		{"set", "${RELAY_TEST_HOST}", "http://ollama:11434"},
		{"set beats default", "${RELAY_TEST_HOST:-http://fallback}", "http://ollama:11434"},
		{"unset takes default", "${RELAY_TEST_UNSET:-http://localhost:11434}", "http://localhost:11434"},
		// An empty variable is treated as unset. Exporting VAR= in a shell is
		// how a variable most often ends up defined-but-blank, and taking that
		// literally would produce an empty base_url, which means "use the
		// provider's public host" — a silent redirection of traffic.
		{"empty takes default", "${RELAY_TEST_EMPTY:-http://localhost:11434}", "http://localhost:11434"},
		{"embedded", "http://${RELAY_TEST_UNSET:-ollama}:11434", "http://ollama:11434"},
		{"unterminated is left alone", "http://${BROKEN", "http://${BROKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandEnv(tc.in); got != tc.want {
				t.Errorf("expandEnv(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpandEnv_UnsetWithNoDefaultIsLeftLiteral is the safety property.
//
// os.Expand would resolve a misspelled variable to the empty string, and an
// empty base_url means "use the adapter's default host" — so a typo would
// quietly send prompts to api.openai.com instead of to the local model the
// author named. Leaving the text as written makes it fail the scheme check
// instead, with the literal in the error.
func TestExpandEnv_UnsetWithNoDefaultIsLeftLiteral(t *testing.T) {
	const in = "${RELAY_DEFINITELY_NOT_SET}"
	if got := expandEnv(in); got != in {
		t.Fatalf("expandEnv(%q) = %q; an unset variable must not resolve to empty", in, got)
	}

	_, err := Load(strings.NewReader(`
version: "t"
endpoints:
  - id: p/m@d
    provider: ollama
    model: m
    deployment: d
    credential_ref: local
    base_url: ${RELAY_DEFINITELY_NOT_SET}
    capabilities: {streaming: true}
    limits: {context_window: 1024, max_output_tokens: 128}
`), Options{})
	if err == nil {
		t.Fatal("an unresolvable base_url loaded successfully")
	}
	if !strings.Contains(err.Error(), "RELAY_DEFINITELY_NOT_SET") {
		t.Errorf("error %q does not name the unresolved variable", err)
	}
}

// TestExpandEnv_AppliesOnlyToBaseURL is the constraint that makes the feature
// safe to have at all.
//
// Every number this product reports is arithmetic over catalog prices, and each
// price carries an attestation the loader enforces. If a price could be
// substituted from the environment, that entire apparatus could be bypassed by
// one variable, with no error and no trace in the file anybody reviews.
func TestExpandEnv_AppliesOnlyToBaseURL(t *testing.T) {
	t.Setenv("RELAY_TEST_PRICE", "0.01")

	_, err := Load(strings.NewReader(`
version: "t"
endpoints:
  - id: p/m@d
    provider: openai
    model: m
    deployment: d
    credential_ref: openai-primary
    capabilities: {streaming: true}
    limits: {context_window: 1024, max_output_tokens: 128}
    pricing:
      input: ${RELAY_TEST_PRICE}
      output: 1.00
      source: test
      verified_on: 2026-08-01
`), Options{})
	if err == nil {
		t.Fatal("a price expanded from the environment; the attestation machinery is bypassable")
	}
}
