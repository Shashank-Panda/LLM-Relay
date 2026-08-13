package server_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/server"
)

// catalogYAML pins prices so the cost assertions below are arithmetic rather
// than approximations. verified_on is far in the future relative to nothing —
// the loader's freshness check is disabled here with MaxPriceAge: 0.
const catalogYAML = `
version: "test-1"
endpoints:
  - id: openai/cheap@us
    provider: openai
    model: cheap-model
    deployment: us
    credential_ref: openai-primary
    capabilities: {streaming: true, tools: true, json_schema: true}
    limits: {context_window: 128000, max_output_tokens: 16384}
    pricing:
      input: 1.00
      output: 2.00
      source: test
      verified_on: 2026-08-01
    quality: {coding: 0.60}
  - id: openai/dear@us
    provider: openai
    model: dear-model
    deployment: us
    credential_ref: openai-primary
    capabilities: {streaming: true, tools: true, json_schema: true}
    limits: {context_window: 200000, max_output_tokens: 64000}
    pricing:
      input: 10.00
      output: 20.00
      source: test
      verified_on: 2026-08-01
    quality: {coding: 0.90}
aliases:
  cheap: openai/cheap@us
routes:
  - name: relay/test
    candidates: [openai/cheap@us, openai/dear@us]
    baseline: openai/dear@us
    weights: {cost: 1.0}
    fallback: openai/cheap@us
`

// harness is Relay in front of a mock provider, exercised over real HTTP.
type harness struct {
	relay    *httptest.Server
	upstream *httptest.Server

	// lastUpstream is the body the provider actually received.
	lastUpstream map[string]any
}

// newHarness builds the full chain: HTTP → wire → gateway → routing → executor
// → adapter → mock provider, and back.
func newHarness(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()
	t.Setenv("RELAY_CRED_OPENAI_PRIMARY", "sk-test")

	h := &harness{}

	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&h.lastUpstream)
		}
		handler(w, r)
	}))
	t.Cleanup(h.upstream.Close)

	cat, err := catalog.Load(strings.NewReader(catalogYAML), catalog.Options{})
	if err != nil {
		t.Fatalf("loading catalog: %v", err)
	}
	// Point every endpoint at the mock. This is what ModelEndpoint.BaseURL is
	// for, and it is the same mechanism a self-hosted or Azure deployment uses.
	for _, ep := range cat.Endpoints {
		ep.BaseURL = h.upstream.URL
	}

	store := catalog.NewStore(cat)
	client := h.upstream.Client()
	registry := provider.NewRegistry(openai.New(client))
	resolver := &provider.EnvResolver{}

	gw := &gateway.Gateway{
		Store:    store,
		Executor: execute.New(registry, resolver),
		Policy:   domain.DefaultPolicy(),
	}

	srv := server.New(gw, store, registry, server.Options{
		Logger: slog.New(slog.DiscardHandler),
	})

	h.relay = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		h.relay.Close()
		client.CloseIdleConnections()
	})

	return h
}

func (h *harness) post(t *testing.T, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(h.relay.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return out
}

func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

const upstreamText = `{
  "id":"chatcmpl-up","model":"cheap-model",
  "choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],
  "usage":{"prompt_tokens":1000,"completion_tokens":500}
}`

func TestNonStreamingEndToEnd(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	resp := h.post(t, `{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	body := decodeBody(t, resp)

	// The model field echoes what the caller asked for — the alias, not the
	// endpoint that served it. Clients key caches and dashboards on this.
	if body["model"] != "cheap" {
		t.Errorf("model = %v, want the requested alias", body["model"])
	}

	choices, _ := body["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello there" {
		t.Errorf("content = %v", msg["content"])
	}

	usage, _ := body["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(1000) || usage["total_tokens"] != float64(1500) {
		t.Errorf("usage = %#v", usage)
	}

	// The alias resolved to a real endpoint, and the provider was asked for
	// that endpoint's provider-side model name.
	if h.lastUpstream["model"] != "cheap-model" {
		t.Errorf("upstream model = %v, want the endpoint's own name", h.lastUpstream["model"])
	}
}

// Phase 1 is strict for everybody: a pinned model is served, never substituted,
// even when a cheaper candidate exists on the route.
func TestStrictModeServesTheRequestedModel(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	resp := h.post(t, `{"model":"openai/dear@us","messages":[{"role":"user","content":"hi"}]}`)

	if got := resp.Header.Get(server.HeaderEndpoint); got != "openai/dear@us" {
		t.Errorf("served endpoint = %q, want the pinned one", got)
	}
	if got := resp.Header.Get(server.HeaderMode); got != string(domain.ModeStrict) {
		t.Errorf("mode = %q, want strict", got)
	}
	// Substitution is only acceptable because it is disclosed. Nothing was
	// substituted here, so the header must be absent rather than "false".
	if got := resp.Header.Get(server.HeaderSubstituted); got != "" {
		t.Errorf("substituted header = %q on a request that was not substituted", got)
	}
	if got := resp.Header.Get(server.HeaderCatalog); got != "test-1" {
		t.Errorf("catalog version = %q", got)
	}
}

func TestUnknownModelIs404WithTheNameSent(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	resp := h.post(t, `{"model":"gpt-9-turbo","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body := decodeBody(t, resp)
	e, _ := body["error"].(map[string]any)
	if e["code"] != "model_not_found" || e["param"] != "model" {
		t.Errorf("error = %#v", e)
	}
	// Naming the model they sent is the difference between a caller fixing
	// their own request and opening a ticket.
	if msg, _ := e["message"].(string); !strings.Contains(msg, "gpt-9-turbo") {
		t.Errorf("message = %q, want it to name the model", msg)
	}
}

func TestMalformedRequestNamesTheField(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	resp := h.post(t, `{"model":"cheap","messages":[{"content":"no role"}]}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	e, _ := decodeBody(t, resp)["error"].(map[string]any)
	if e["param"] != "messages[0].role" {
		t.Errorf("param = %v, want the offending field", e["param"])
	}
}

// An upstream failure surfaces with the provider's own message. Paraphrasing it
// turns a self-service fix into a support ticket.
func TestProviderErrorSurfacesVerbatim(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Header().Set("Retry-After", "3")
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached for cheap-model","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	})

	resp := h.post(t, `{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the provider's 429", resp.StatusCode)
	}
	e, _ := decodeBody(t, resp)["error"].(map[string]any)
	if msg, _ := e["message"].(string); !strings.Contains(msg, "Rate limit reached") {
		t.Errorf("message = %q, want the provider's own text", msg)
	}
	// The credential must never reach the caller or a log.
	if msg, _ := e["message"].(string); strings.Contains(msg, "sk-") {
		t.Fatalf("error leaked a credential: %q", msg)
	}
}

const upstreamStream = `data: {"id":"c","model":"cheap-model","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"c","model":"cheap-model","choices":[{"index":0,"delta":{"content":"Hello"}}]}

data: {"id":"c","model":"cheap-model","choices":[{"index":0,"delta":{"content":" there"}}]}

data: {"id":"c","model":"cheap-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c","model":"cheap-model","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":500}}

data: [DONE]

`

func sseHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

// readFrames collects the data payload of every SSE frame.
func readFrames(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" && strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
		if err != nil {
			return out
		}
	}
}

func TestStreamingEndToEnd(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	h := newHarness(t, sseHandler(upstreamStream))

	resp := h.post(t, `{"model":"cheap","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	// Without this a buffering proxy turns streaming into a slow non-streaming
	// response, with no error anywhere.
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	// Disclosure headers must be set before the first frame, because after it
	// they are already on the wire.
	if got := resp.Header.Get(server.HeaderEndpoint); got != "openai/cheap@us" {
		t.Errorf("endpoint header = %q", got)
	}

	frames := readFrames(t, resp.Body)

	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE]; every SDK waits for it", frames[len(frames)-1])
	}

	// The opening frame carries only the role, so incremental clients have an
	// object to apply deltas to.
	if !strings.Contains(frames[0], `"role":"assistant"`) {
		t.Errorf("first frame = %s, want the role opener", frames[0])
	}

	var text strings.Builder
	var sawUsage bool
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens int `json:"prompt_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(f), &chunk); err != nil {
			t.Fatalf("frame %s: %v", f, err)
		}
		if chunk.Model != "cheap" {
			t.Errorf("chunk model = %q, want the requested alias", chunk.Model)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != nil {
				text.WriteString(*c.Delta.Content)
			}
		}
		if chunk.Usage != nil {
			sawUsage = true
			if chunk.Usage.TotalTokens != 1500 {
				t.Errorf("usage total = %d, want 1500", chunk.Usage.TotalTokens)
			}
		}
	}

	if text.String() != "Hello there" {
		t.Errorf("reassembled text = %q", text.String())
	}
	if !sawUsage {
		t.Error("no usage frame; streamed requests would all record estimated cost")
	}
}

// TestStreamFramesArriveIncrementally is the property that makes streaming
// worth anything. A response flushed only at the end is a slow non-streaming
// response wearing an SSE content type.
func TestStreamFramesArriveIncrementally(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		_, _ = io.WriteString(w, `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"first"}}]}`+"\n\n")
		fl.Flush()

		<-release

		_, _ = io.WriteString(w, `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	})

	resp := h.post(t, `{"model":"cheap","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()

	// The first content frame must be readable while the provider is still
	// holding the connection open.
	got := make(chan string, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if strings.Contains(line, `"first"`) {
				got <- line
				return
			}
			if err != nil {
				got <- ""
				return
			}
		}
	}()

	select {
	case line := <-got:
		if line == "" {
			t.Fatal("stream ended before the first content frame")
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("the first frame did not arrive while the provider was still streaming — " +
			"the response is being buffered, which is not streaming")
	}

	close(release)
	_, _ = io.Copy(io.Discard, resp.Body)
}

// TestClientDisconnectAbortsUpstream is the most expensive bug a gateway can
// have. Without cancellation propagation Relay keeps generating — and paying
// for — tokens nobody will read, and nothing in any log or metric says so.
func TestClientDisconnectAbortsUpstream(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	aborted := make(chan struct{})
	done := make(chan struct{})

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		_, _ = io.WriteString(w, `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"tok"}}]}`+"\n\n")
		fl.Flush()

		select {
		case <-r.Context().Done():
			close(aborted)
		case <-time.After(5 * time.Second):
		}
	})

	req, err := http.NewRequest(http.MethodPost, h.relay.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"cheap","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// A dedicated client, so closing its idle connections tears down only this
	// request's socket and the disconnect is unambiguous.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	// Read the first frame so the stream is genuinely established, then hang up
	// the way a real client does.
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("reading first frame: %v", err)
	}
	resp.Body.Close()
	client.CloseIdleConnections()

	select {
	case <-aborted:
	case <-time.After(4 * time.Second):
		t.Fatal("the provider never saw the cancellation — Relay is still paying " +
			"for tokens the client will never receive")
	}
	<-done
}

func TestModelsListsEndpointsAliasesAndRoutes(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	resp, err := http.Get(h.relay.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if body.Object != "list" {
		t.Errorf("object = %q", body.Object)
	}

	ids := map[string]bool{}
	for _, m := range body.Data {
		ids[m.ID] = true
	}
	// All three are things a caller may legitimately put in the model field.
	for _, want := range []string{"openai/cheap@us", "cheap", "relay/test"} {
		if !ids[want] {
			t.Errorf("%q missing from /v1/models", want)
		}
	}
}

func TestHealthAndReadiness(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(h.relay.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	resp, err := http.Get(h.relay.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Method-qualified mux patterns give this for free, with no per-handler
	// check that somebody eventually forgets to write.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRequestIDIsEchoed(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	t.Run("generated when absent", func(t *testing.T) {
		resp := h.post(t, `{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`)
		if resp.Header.Get(server.HeaderRequestID) == "" {
			t.Error("no request ID was assigned")
		}
	})

	t.Run("adopted when supplied", func(t *testing.T) {
		// A caller's own correlation ID must survive, so their traces and
		// Relay's line up.
		req, _ := http.NewRequest(http.MethodPost, h.relay.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(server.HeaderRequestID, "caller-supplied-id")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()

		if got := resp.Header.Get(server.HeaderRequestID); got != "caller-supplied-id" {
			t.Errorf("request ID = %q, want the caller's", got)
		}
	})
}

func TestBodySizeLimit(t *testing.T) {
	h := newHarness(t, jsonHandler(upstreamText))

	// Enforced before parsing: a limit applied to an already-decoded document
	// has already paid the memory cost it exists to prevent.
	huge := fmt.Sprintf(`{"model":"cheap","messages":[{"role":"user","content":%q}]}`,
		strings.Repeat("x", 11<<20))

	resp := h.post(t, huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// A tool-calling request must survive the round trip through the neutral model
// and back out in the vendor's shape.
func TestToolCallingRoundTrip(t *testing.T) {
	const upstreamToolCall = `{
	  "id":"c","model":"cheap-model",
	  "choices":[{"index":0,"message":{"role":"assistant","content":null,
	    "tool_calls":[{"id":"call_1","type":"function",
	      "function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
	    "finish_reason":"tool_calls"}],
	  "usage":{"prompt_tokens":10,"completion_tokens":5}
	}`

	h := newHarness(t, jsonHandler(upstreamToolCall))

	resp := h.post(t, `{
		"model":"cheap",
		"messages":[{"role":"user","content":"weather in Paris?"}],
		"tools":[{"type":"function","function":{"name":"get_weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]
	}`)

	// The tool definition reached the provider.
	tools, _ := h.lastUpstream["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("upstream tools = %#v", h.lastUpstream["tools"])
	}

	body := decodeBody(t, resp)
	choices, _ := body["choices"].([]any)
	c, _ := choices[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)

	// A pure tool-call response carries content:null. Emitting "" instead makes
	// it look like an empty answer, and clients that check for falsy content
	// treat that as a refusal.
	if content, present := msg["content"]; !present || content != nil {
		t.Errorf("content = %#v, want null", content)
	}
	if c["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", c["finish_reason"])
	}

	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", msg["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Paris"}` {
		t.Errorf("function = %#v", fn)
	}
}
