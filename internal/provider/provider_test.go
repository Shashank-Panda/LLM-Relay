package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// TestCredentialRedacts is a security property, not a formatting preference.
// Anything that prints a Credential — a log line, a wrapped error, a panic dump
// — must not print the secret. Relying on every future call site to remember is
// not a control.
func TestCredentialRedacts(t *testing.T) {
	c := Credential{Ref: "anthropic-primary", APIKey: "sk-ant-secret-value"}

	for _, format := range []string{"%v", "%s", "%+v", "%#v"} {
		got := fmt.Sprintf(format, c)
		if got != "credential(anthropic-primary)" {
			t.Errorf("%s printed %q", format, got)
		}
	}

	// The same must hold when it is a struct field somewhere.
	wrapper := struct{ Cred Credential }{c}
	if got := fmt.Sprintf("%v", wrapper); got != "{credential(anthropic-primary)}" {
		t.Errorf("nested credential printed %q", got)
	}
}

// epFor is the minimal endpoint a resolver needs. Resolvers take the endpoint
// rather than the bare ref because a stored credential may be scoped per
// deployment, and nothing here is allowed to read anything else off it.
func epFor(ref string) *domain.ModelEndpoint {
	return &domain.ModelEndpoint{ID: "p/m@d", CredentialRef: ref}
}

func TestEnvResolver(t *testing.T) {
	t.Setenv("RELAY_CRED_ANTHROPIC_PRIMARY", "sk-test")

	r := &EnvResolver{Free: map[string]bool{"local": true}}

	t.Run("reads the derived variable", func(t *testing.T) {
		c, err := r.Resolve(context.Background(), "", epFor("anthropic-primary"))
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if c.APIKey != "sk-test" || c.Ref != "anthropic-primary" {
			t.Errorf("got %+v", c.Ref)
		}
	})

	t.Run("a free ref needs no secret", func(t *testing.T) {
		if _, err := r.Resolve(context.Background(), "", epFor("local")); err != nil {
			t.Errorf("Resolve(local): %v", err)
		}
	})

	t.Run("a missing secret names the variable to set", func(t *testing.T) {
		_, err := r.Resolve(context.Background(), "", epFor("openai-primary"))
		var missing *ErrNoCredential
		if !errors.As(err, &missing) {
			t.Fatalf("err = %v, want *ErrNoCredential", err)
		}
		if missing.EnvVar != "RELAY_CRED_OPENAI_PRIMARY" {
			t.Errorf("EnvVar = %q", missing.EnvVar)
		}
	})
}

func TestEnvVarFor(t *testing.T) {
	r := &EnvResolver{}
	tests := map[string]string{
		"anthropic-primary": "RELAY_CRED_ANTHROPIC_PRIMARY",
		"openai.eu":         "RELAY_CRED_OPENAI_EU",
		"team/shared":       "RELAY_CRED_TEAM_SHARED",
	}
	for ref, want := range tests {
		if got := r.EnvVarFor(ref); got != want {
			t.Errorf("EnvVarFor(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		status int
		want   ErrorClass
	}{
		{429, ClassRetrySame},
		{500, ClassRetrySame},
		{502, ClassRetrySame},
		{503, ClassRetrySame},
		{504, ClassRetrySame},
		{408, ClassRetrySame},
		// A malformed request fails identically at every provider. Retrying it
		// across three produces three bills and one guaranteed failure.
		{400, ClassTerminal},
		{401, ClassTerminal},
		{403, ClassTerminal},
		{404, ClassTerminal},
		{422, ClassTerminal},
	}
	for _, tc := range tests {
		if got := classifyStatus(tc.status); got != tc.want {
			t.Errorf("classifyStatus(%d) = %s, want %s", tc.status, got, tc.want)
		}
	}
}

func TestClassifyTransport(t *testing.T) {
	if got := ClassifyTransport(context.Canceled); got != ClassCancelled {
		t.Errorf("cancelled = %s", got)
	}
	if got := ClassifyTransport(context.DeadlineExceeded); got != ClassCancelled {
		t.Errorf("deadline = %s", got)
	}
	// A wrapped cancellation is still a cancellation — the transport layers
	// wrap it in url.Error before it reaches us.
	wrapped := fmt.Errorf("Post %q: %w", "https://x", context.Canceled)
	if got := ClassifyTransport(wrapped); got != ClassCancelled {
		t.Errorf("wrapped cancel = %s", got)
	}
	if got := ClassifyTransport(errors.New("connection reset by peer")); got != ClassRetrySame {
		t.Errorf("reset = %s", got)
	}
}

// TestClassOfDefaultsTerminal pins the conservative default. An unrecognised
// failure that gets retried costs money for nothing; one that fails fast costs
// a request. For a cost-reduction product the tie goes to not spending.
func TestClassOfDefaultsTerminal(t *testing.T) {
	if got := ClassOf(errors.New("who knows")); got != ClassTerminal {
		t.Errorf("ClassOf(unknown) = %s, want Terminal", got)
	}
	if got := ClassOf(nil); got != ClassTerminal {
		t.Errorf("ClassOf(nil) = %s, want Terminal", got)
	}

	pe := &Error{Class: ClassReroute}
	if got := ClassOf(fmt.Errorf("wrapped: %w", pe)); got != ClassReroute {
		t.Errorf("ClassOf(wrapped *Error) = %s, want Reroute", got)
	}
}

func TestHTTPStatusOf(t *testing.T) {
	// Cancelled requests must not land in the 5xx error budget: the client
	// hanging up is not Relay failing.
	if got := HTTPStatusOf(context.Canceled); got != 499 {
		t.Errorf("cancelled status = %d, want 499", got)
	}
	if got := HTTPStatusOf(&Error{Class: ClassRetrySame}); got != http.StatusBadGateway {
		t.Errorf("retryable status = %d, want 502", got)
	}
	if got := HTTPStatusOf(&Error{Class: ClassTerminal, HTTPStatus: 401}); got != 401 {
		t.Errorf("terminal status = %d, want the provider's 401", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}

	h.Set("Retry-After", "30")
	if got := ParseRetryAfter(h); got != 30*time.Second {
		t.Errorf("seconds form = %v", got)
	}

	h.Set("Retry-After", http.TimeFormat[:0]+time.Now().Add(time.Minute).UTC().Format(http.TimeFormat))
	if got := ParseRetryAfter(h); got <= 0 || got > time.Minute {
		t.Errorf("date form = %v, want just under a minute", got)
	}

	// A date in the past means "now", not a negative sleep.
	h.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	if got := ParseRetryAfter(h); got != 0 {
		t.Errorf("past date = %v, want 0", got)
	}

	h.Del("Retry-After")
	if got := ParseRetryAfter(h); got != 0 {
		t.Errorf("absent = %v, want 0", got)
	}
}

func TestExtractMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"openai shape", `{"error":{"message":"bad model","type":"invalid_request_error"}}`,
			"invalid_request_error: bad model"},
		{"anthropic shape", `{"error":{"type":"overloaded_error","message":"Overloaded"}}`,
			"overloaded_error: Overloaded"},
		{"ollama shape", `{"error":"model not found"}`, `{"error":"model not found"}`},
		{"bare message", `{"message":"nope"}`, "nope"},
		// A load balancer returning HTML still tells you something useful:
		// the request never reached the provider.
		{"html from a proxy", "<html>502 Bad Gateway</html>", "<html>502 Bad Gateway</html>"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestUsageCostDeductsCachedTokens guards a double-charge. Every provider
// reports cached tokens as a subset of input tokens, so pricing both counts at
// full rate overstates cost by exactly the cached fraction — in the direction
// that would inflate Relay's own savings figures.
func TestUsageCostDeductsCachedTokens(t *testing.T) {
	ep := &domain.ModelEndpoint{
		Pricing: domain.Pricing{
			Input:       3_000_000, // $3.00 / 1M
			CachedInput: 300_000,   // $0.30 / 1M
			Output:      15_000_000,
		},
	}

	u := Usage{InputTokens: 100_000, CachedInputTokens: 90_000, OutputTokens: 1_000}

	// 10k uncached at $3 = $0.03; 90k cached at $0.30 = $0.027; 1k out at $15 = $0.015
	want := domain.Money(30_000 + 27_000 + 15_000)
	if got := u.Cost(ep); got != want {
		t.Errorf("Cost = %s, want %s", got, want)
	}

	t.Run("no cached rate declared prices everything at full rate", func(t *testing.T) {
		plain := &domain.ModelEndpoint{Pricing: domain.Pricing{Input: 3_000_000}}
		u := Usage{InputTokens: 100_000, CachedInputTokens: 90_000}
		if got := u.Cost(plain); got != 300_000 {
			t.Errorf("Cost = %s, want $0.30", got)
		}
	})

	t.Run("cached exceeding input does not go negative", func(t *testing.T) {
		u := Usage{InputTokens: 10, CachedInputTokens: 1_000_000}
		if got := u.Cost(ep); got < 0 {
			t.Errorf("Cost = %s, want a non-negative amount", got)
		}
	})
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(&fakeAdapter{id: "ollama"}, &fakeAdapter{id: "anthropic"})

	if _, err := r.For("ollama"); err != nil {
		t.Errorf("For(ollama): %v", err)
	}

	var missing *ErrNoAdapter
	if _, err := r.For("openai"); !errors.As(err, &missing) {
		t.Errorf("For(openai) err = %v, want *ErrNoAdapter", err)
	}

	if got := r.Providers(); len(got) != 2 || got[0] != "anthropic" || got[1] != "ollama" {
		t.Errorf("Providers() = %v, want sorted [anthropic ollama]", got)
	}
}

// TestRegistryUnreachable catches a config mistake at startup rather than at
// request time: an endpoint routing can select but execution cannot reach is a
// latent outage sitting in a YAML file.
func TestRegistryUnreachable(t *testing.T) {
	cat := &domain.Catalog{
		Endpoints: map[string]*domain.ModelEndpoint{
			"a": {ID: "a", Provider: "ollama"},
			"b": {ID: "b", Provider: "openai"},
			"c": {ID: "c", Provider: "cohere"},
		},
	}
	r := NewRegistry(&fakeAdapter{id: "ollama"})

	got := r.Unreachable(cat)
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("Unreachable = %v, want [b c]", got)
	}
}

type fakeAdapter struct{ id domain.ProviderID }

func (f *fakeAdapter) ID() domain.ProviderID { return f.id }
func (f *fakeAdapter) Chat(context.Context, *domain.NormalizedRequest, *domain.ModelEndpoint, Credential) (*Response, error) {
	return nil, nil
}
func (f *fakeAdapter) ChatStream(context.Context, *domain.NormalizedRequest, *domain.ModelEndpoint, Credential) (Stream, error) {
	return nil, nil
}
func (f *fakeAdapter) ClassifyError(r *http.Response, err error) ErrorClass {
	return DefaultClassify(r, err)
}
