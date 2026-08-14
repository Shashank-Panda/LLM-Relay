// Package providertest is the contract every adapter must satisfy.
//
// Its purpose is to make "normalized" mean one thing. Each adapter supplies
// recorded response bodies in its own vendor's format; the suite asserts they
// all produce the *same* neutral result. A vendor difference that survives into
// the domain model is a bug the individual adapter's own tests cannot see,
// because they only ever look at one vendor.
//
// Adding a provider therefore means writing fixtures rather than writing tests,
// which is the difference between provider coverage growing and not.
package providertest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// The single set of expected values every adapter's fixtures must produce.
// Hard-coded rather than supplied per adapter: if each vendor could declare its
// own expectations, the suite would verify that each adapter agrees with itself
// rather than that they agree with each other.
const (
	WantText     = "Hello there"
	WantToolName = "get_weather"
	WantToolArgs = `{"city":"Paris"}`

	// WantUnicode exists to be split across chunk boundaries mid-rune. A reader
	// that slices bytes without regard for UTF-8 turns these into replacement
	// characters, and the corruption is invisible in ASCII-only tests.
	WantUnicode = "héllo 🌍 café"

	WantInputTokens  = 10
	WantOutputTokens = 5
)

// Fixtures are recorded provider responses, verbatim on the wire.
type Fixtures struct {
	// Text is a non-streaming plain answer producing WantText and the token
	// counts above.
	Text string

	// TextStream produces WantText across several frames.
	TextStream string

	// ToolCall is a non-streaming response calling WantToolName with
	// WantToolArgs.
	ToolCall string

	// ToolCallStream is the same call with its arguments split across delta
	// boundaries at points that do not align with JSON tokens — mid-key,
	// mid-string. This is the case that breaks naive implementations, and it is
	// what providers actually send.
	ToolCallStream string

	// UnicodeStream carries WantUnicode with at least one multi-byte rune split
	// across two frames.
	UnicodeStream string

	// EmptyStream terminates with no content at all. Providers do this on a
	// filtered or immediately-stopped generation, and it must not be reported
	// as an error.
	EmptyStream string
}

// Suite configures a run.
type Suite struct {
	Name string

	// New builds the adapter under test against the supplied client.
	New func(*http.Client) provider.Adapter

	// Endpoint is the catalog entry; the suite overwrites BaseURL to point at
	// its own test server.
	Endpoint *domain.ModelEndpoint

	Fixtures Fixtures

	// StatusClasses overrides the expected classification for a status code
	// where the vendor's meaning differs from the default — an Ollama 404 means
	// "model not pulled here", which is a Reroute rather than a Terminal.
	StatusClasses map[int]provider.ErrorClass

	// ContentType is the response content type the mock server sends. Defaults
	// to application/json for non-streaming and text/event-stream for streams.
	StreamContentType string
}

// Run executes the full contract.
func Run(t *testing.T, s Suite) {
	t.Helper()
	t.Run(s.Name, func(t *testing.T) {
		t.Run("non-streaming text", s.testText)
		t.Run("non-streaming tool call", s.testToolCall)
		t.Run("streaming text", s.testTextStream)
		t.Run("streaming tool call reassembly", s.testToolCallStream)
		t.Run("streaming unicode across boundaries", s.testUnicodeStream)
		t.Run("streaming empty completion", s.testEmptyStream)
		t.Run("error classification", s.testErrorClasses)
		t.Run("cancellation reaches the provider", s.testCancellation)
		t.Run("close is idempotent", s.testCloseTwice)
	})
}

// checkLeaks arms a goroutine-leak assertion for this test.
//
// Registered before anything else, because t.Cleanup runs LIFO: this must fire
// after the mock server and the connection pool have been torn down, or it
// reports their perfectly ordinary background goroutines as leaks. A deferred
// call cannot be used at all — deferred functions run before cleanups, so the
// server would still be listening when the check ran.
func checkLeaks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { goleak.VerifyNone(t) })
}

// serve starts a mock provider returning body, and returns the adapter and
// endpoint wired to it.
func (s Suite) serve(t *testing.T, body string, streaming bool) (provider.Adapter, *domain.ModelEndpoint) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if streaming {
			ct := s.StreamContentType
			if ct == "" {
				ct = "text/event-stream"
			}
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		// Written in one shot. The framing tests care about how the adapter
		// parses frames, not about how TCP happened to segment them.
		_, _ = io.WriteString(w, body)
	}))

	a, ep, client := s.adapterFor(srv.URL)
	t.Cleanup(func() {
		srv.Close()
		// Pooled connections hold a read and a write goroutine each for up to
		// IdleConnTimeout. Harmless in production, indistinguishable from a
		// leak in a test that checks for one.
		client.CloseIdleConnections()
	})
	return a, ep
}

func (s Suite) adapterFor(baseURL string) (provider.Adapter, *domain.ModelEndpoint, *http.Client) {
	client := provider.NewClient(provider.ClientOptions{ResponseHeaderTimeout: 5 * time.Second})
	ep := *s.Endpoint
	ep.BaseURL = baseURL
	return s.New(client), &ep, client
}

func request() *domain.NormalizedRequest {
	return &domain.NormalizedRequest{
		ID:        "req-test",
		RouteName: "relay/test",
		Messages: []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.ContentPart{{Kind: domain.PartText, Text: "hi"}},
		}},
	}
}

func (s Suite) testText(t *testing.T) {
	a, ep := s.serve(t, s.Fixtures.Text, false)

	resp, err := a.Chat(context.Background(), request(), ep, provider.Credential{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if got := textOf(resp.Parts); got != WantText {
		t.Errorf("text = %q, want %q", got, WantText)
	}
	if resp.FinishReason != provider.FinishStop {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage.InputTokens != WantInputTokens || resp.Usage.OutputTokens != WantOutputTokens {
		t.Errorf("Usage = %+v, want %d in / %d out",
			resp.Usage, WantInputTokens, WantOutputTokens)
	}
	if resp.Usage.Estimated {
		t.Error("Usage.Estimated is true although the provider reported counts")
	}
}

func (s Suite) testToolCall(t *testing.T) {
	a, ep := s.serve(t, s.Fixtures.ToolCall, false)

	resp, err := a.Chat(context.Background(), request(), ep, provider.Credential{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	calls := callsOf(resp.Parts)
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ToolName != WantToolName {
		t.Errorf("ToolName = %q, want %q", calls[0].ToolName, WantToolName)
	}
	if !sameJSON(calls[0].Arguments, WantToolArgs) {
		t.Errorf("Arguments = %q, want %q", calls[0].Arguments, WantToolArgs)
	}
	// Clients match results back to calls by ID on the next turn. An empty ID
	// makes a multi-call turn unresolvable.
	if calls[0].ToolCallID == "" {
		t.Error("tool call has no ID")
	}
	if resp.FinishReason != provider.FinishToolCalls {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
}

func (s Suite) testTextStream(t *testing.T) {
	checkLeaks(t)

	a, ep := s.serve(t, s.Fixtures.TextStream, true)

	got, _, usage := collect(t, a, ep)
	if got.text != WantText {
		t.Errorf("streamed text = %q, want %q", got.text, WantText)
	}
	if got.finish != provider.FinishStop {
		t.Errorf("FinishReason = %q, want stop", got.finish)
	}
	if usage.InputTokens != WantInputTokens || usage.OutputTokens != WantOutputTokens {
		t.Errorf("Usage = %+v, want %d in / %d out", usage, WantInputTokens, WantOutputTokens)
	}
}

// testToolCallStream is the case this suite exists for.
//
// Argument fragments arrive split at arbitrary byte offsets — mid-key,
// mid-string, sometimes mid-escape — and only the concatenation is valid JSON.
// An adapter that parses each fragment, or that reorders them, produces a tool
// call the client cannot execute.
func (s Suite) testToolCallStream(t *testing.T) {
	checkLeaks(t)

	a, ep := s.serve(t, s.Fixtures.ToolCallStream, true)

	got, calls, _ := collect(t, a, ep)
	_ = got

	if len(calls) != 1 {
		t.Fatalf("reassembled %d calls, want 1: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.name != WantToolName {
		t.Errorf("name = %q, want %q", c.name, WantToolName)
	}
	if !sameJSON(c.args, WantToolArgs) {
		t.Errorf("reassembled arguments = %q, want %q", c.args, WantToolArgs)
	}
	// Identity arrives once, on the opening delta. Repeating it on every
	// fragment is a shape some SDKs assemble into duplicate calls.
	if c.identityDeltas != 1 {
		t.Errorf("tool identity appeared on %d deltas, want exactly 1", c.identityDeltas)
	}
	if c.id == "" {
		t.Error("streamed tool call has no ID")
	}
}

func (s Suite) testUnicodeStream(t *testing.T) {
	checkLeaks(t)

	a, ep := s.serve(t, s.Fixtures.UnicodeStream, true)

	got, _, _ := collect(t, a, ep)
	if got.text != WantUnicode {
		t.Errorf("streamed text = %q, want %q\n"+
			"a multi-byte rune was corrupted across a frame boundary", got.text, WantUnicode)
	}
}

// An empty completion is a completion. Providers return one on a filtered or
// immediately-stopped generation, and reporting it as an error would turn a
// valid if useless answer into a failed request — and, with retries enabled
// later, into three of them.
func (s Suite) testEmptyStream(t *testing.T) {
	checkLeaks(t)

	a, ep := s.serve(t, s.Fixtures.EmptyStream, true)

	got, calls, _ := collect(t, a, ep)
	if got.text != "" {
		t.Errorf("text = %q, want empty", got.text)
	}
	if len(calls) != 0 {
		t.Errorf("got %d tool calls, want none", len(calls))
	}
}

func (s Suite) testErrorClasses(t *testing.T) {
	cases := map[int]provider.ErrorClass{
		429: provider.ClassRetrySame,
		500: provider.ClassRetrySame,
		502: provider.ClassRetrySame,
		503: provider.ClassRetrySame,
		400: provider.ClassTerminal,
		401: provider.ClassTerminal,
		403: provider.ClassTerminal,
		404: provider.ClassTerminal,
	}
	for status, class := range s.StatusClasses {
		cases[status] = class
	}

	for status, want := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"test_error"}}`)
			}))
			defer srv.Close()

			a, ep, client := s.adapterFor(srv.URL)
			defer client.CloseIdleConnections()

			_, err := a.Chat(context.Background(), request(), ep, provider.Credential{})
			if err == nil {
				t.Fatalf("status %d produced no error", status)
			}

			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want *provider.Error", err, err)
			}
			if pe.Class != want {
				t.Errorf("class = %s, want %s", pe.Class, want)
			}
			if pe.HTTPStatus != status {
				t.Errorf("HTTPStatus = %d, want %d", pe.HTTPStatus, status)
			}
			// The provider's own message is the useful one. Hiding it turns a
			// self-service fix into a support ticket.
			if !strings.Contains(pe.Message, "boom") {
				t.Errorf("Message = %q, want the provider's text", pe.Message)
			}
			// A stated backoff is honoured verbatim; substituting our own is
			// guessing against a number the provider already knows.
			if pe.RetryAfter != 7*time.Second {
				t.Errorf("RetryAfter = %v, want 7s", pe.RetryAfter)
			}
			// The error will be logged. It must never carry a secret.
			if strings.Contains(err.Error(), "sk-") {
				t.Errorf("error text leaked a credential: %s", err)
			}
		})
	}
}

// testCancellation is the most expensive bug a gateway can have.
//
// When the client disconnects, the upstream call must abort. Without it Relay
// keeps generating — and paying for — tokens nobody will read, and nothing in
// any metric or log says so. It only shows up on the invoice.
func (s Suite) testCancellation(t *testing.T) {
	checkLeaks(t)

	aborted := make(chan struct{})
	released := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := s.StreamContentType
		if ct == "" {
			ct = "text/event-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Hold the response open until the client's cancellation propagates.
		select {
		case <-r.Context().Done():
			close(aborted)
		case <-released:
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(released)

	a, ep, client := s.adapterFor(srv.URL)
	defer client.CloseIdleConnections()

	ctx, cancel := context.WithCancel(context.Background())

	st, err := a.ChatStream(ctx, request(), ep, provider.Credential{})
	if err != nil {
		cancel()
		t.Fatalf("ChatStream: %v", err)
	}
	defer st.Close()

	done := make(chan error, 1)
	go func() {
		_, err := st.Recv()
		done <- err
	}()

	cancel()

	select {
	case <-aborted:
	case <-time.After(3 * time.Second):
		t.Fatal("the provider never saw the cancellation — the upstream request " +
			"was not derived from the caller's context, so tokens keep being billed")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("Recv returned no error after cancellation")
		}
		if err != io.EOF && provider.ClassOf(err) != provider.ClassCancelled {
			t.Errorf("Recv error classified %s, want Cancelled", provider.ClassOf(err))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Recv did not return after cancellation")
	}
}

// Close runs on every path including panic recovery, so it runs twice more
// often than anyone expects. A second Close must not panic or error.
func (s Suite) testCloseTwice(t *testing.T) {
	a, ep := s.serve(t, s.Fixtures.TextStream, true)

	st, err := a.ChatStream(context.Background(), request(), ep, provider.Credential{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
