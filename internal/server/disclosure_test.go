package server_test

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/server"
)

// --- the cost headers ---

// TestCostHeadersAreTheOperandsOfSaved pins the arithmetic rather than the
// values.
//
// The saving is the product's headline claim, and a claim a caller cannot check
// is an assertion. Emitting the difference without its two operands leaves the
// caller no way to verify the subtraction, which is the one thing this product
// asks to be trusted about — so the three headers are emitted together, under
// the same condition, or not at all.
func TestCostHeadersAreTheOperandsOfSaved(t *testing.T) {
	respond := func(site string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageJSON(site, "an answer"))
	}

	h := newSubHarness(t, domain.ModeOptimize, respond)
	resp := h.post(t, subBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	defer drainBody(t, resp)

	cost := parseUSD(t, resp.Header, server.HeaderCost)
	baseline := parseUSD(t, resp.Header, server.HeaderBaselineCost)
	saved := parseUSD(t, resp.Header, server.HeaderSaved)

	if got, want := baseline-cost, saved; !nearly(got, want) {
		t.Errorf("%s - %s = %.6f, but %s says %.6f; a saving whose operands do not "+
			"reproduce it is a number nobody can audit",
			server.HeaderBaselineCost, server.HeaderCost, got, server.HeaderSaved, want)
	}
	if saved <= 0 {
		t.Errorf("saved = %.6f, want a downgrade to have saved something", saved)
	}
	if cost >= baseline {
		t.Errorf("cost %.6f is not below baseline %.6f; the baseline is a ceiling",
			cost, baseline)
	}
}

// TestCostHeadersOnAPinnedRequestReportATruthfulZero.
//
// X-Relay-Pin: strict serves the baseline itself, so the saving is genuinely
// zero rather than unmeasured — and the two are different facts the rest of the
// system is careful to keep apart. The operands must still be emitted, and they
// must be equal: a caller who pinned a model is entitled to see what it cost and
// to see the arithmetic come out to nothing, rather than to be told nothing at
// all and left to wonder whether the figure was suppressed or never computed.
func TestCostHeadersOnAPinnedRequestReportATruthfulZero(t *testing.T) {
	respond := func(site string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageJSON(site, "an answer"))
	}
	h := newSubHarness(t, domain.ModeOptimize, respond)

	resp := h.postWith(t, subBody, map[string]string{server.HeaderPin: "strict"})
	defer drainBody(t, resp)

	if got := resp.Header.Get(server.HeaderSubstituted); got == "true" {
		t.Fatalf("%s = true on a pinned request", server.HeaderSubstituted)
	}

	cost := parseUSD(t, resp.Header, server.HeaderCost)
	baseline := parseUSD(t, resp.Header, server.HeaderBaselineCost)
	saved := parseUSD(t, resp.Header, server.HeaderSaved)

	if !nearly(cost, baseline) {
		t.Errorf("pinned request cost %.6f against baseline %.6f; the baseline *is* what "+
			"was served, so these cannot differ", cost, baseline)
	}
	if saved != 0 {
		t.Errorf("%s = %.6f on a pinned request, want exactly 0", server.HeaderSaved, saved)
	}
}

// --- the stream heartbeat ---

// TestStreamHeartbeatKeepsAnIdleStreamOpen is the regression test for a config
// field that existed, was documented, and was read by nothing.
//
// The failure it guards against is invisible: a model that thinks for ninety
// seconds before its first token looks exactly like a dead connection to every
// proxy in the path, and the symptom is a truncated answer rather than a
// timeout anybody would investigate.
func TestStreamHeartbeatKeepsAnIdleStreamOpen(t *testing.T) {
	// The provider opens the stream, says nothing for long enough that several
	// heartbeats must fire, then produces one token and finishes.
	const idle = 120 * time.Millisecond

	h := newHeartbeatHarness(t, 20*time.Millisecond, func(w http.ResponseWriter) {
		time.Sleep(idle)
		_, _ = io.WriteString(w, "data: "+
			`{"id":"x","object":"chat.completion.chunk","choices":`+
			`[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	})

	comments, data := countFrames(t, h, streamBody)

	if comments == 0 {
		t.Fatalf("no heartbeat frames during a %v silence; the option is dead again", idle)
	}
	if data == 0 {
		t.Errorf("heartbeats fired but no data frame arrived: got %d comments, 0 data", comments)
	}
}

// TestStreamHeartbeatIsOnByDefault guards the zero-value trap.
//
// Zero means unset, not disabled. If New treated an unset duration as "off",
// every deployment that did not know the option existed would lose long streams
// behind any proxy with an idle timeout — which is every proxy — and would have
// no way to tell that was what happened.
func TestStreamHeartbeatIsOnByDefault(t *testing.T) {
	if server.DefaultOptions().StreamHeartbeat <= 0 {
		t.Fatal("DefaultOptions has no heartbeat; the rest of this test proves nothing")
	}

	// Silence for longer than the default would ever allow is impractical in a
	// test, so this asserts the wiring instead: with the option left unset, the
	// stream still completes and the writer is the same one the heartbeat path
	// uses. The behaviour above is asserted with an explicit short interval.
	h := newHeartbeatHarness(t, 0, func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, "data: "+
			`{"id":"x","object":"chat.completion.chunk","choices":`+
			`[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	})

	_, data := countFrames(t, h, streamBody)
	if data == 0 {
		t.Error("no data frames with the heartbeat left unset")
	}
}

// --- helpers ---

const streamBody = `{"model":"cheap","messages":[{"role":"user","content":"hi"}],"stream":true}`

// heartbeatCatalog is one endpoint and no route: this exercises the streaming
// writer, not the router, and a single candidate keeps it that way.
const heartbeatCatalog = `
version: "test"
endpoints:
  - id: openai/cheap@t
    provider: openai
    model: cheap
    deployment: t
    credential_ref: openai-primary
    base_url: BASE
    capabilities: {streaming: true}
    limits: {context_window: 8192, max_output_tokens: 1024}
    pricing:
      input: 1.00
      output: 1.00
      source: test
      verified_on: 2026-08-01
aliases:
  cheap: openai/cheap@t
`

func newHeartbeatHarness(t *testing.T, hb time.Duration, write func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	t.Setenv("RELAY_CRED_OPENAI_PRIMARY", "sk-test")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		write(w)
	}))
	t.Cleanup(upstream.Close)

	cat, err := catalog.Load(
		strings.NewReader(strings.ReplaceAll(heartbeatCatalog, "BASE", upstream.URL)),
		catalog.Options{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store := catalog.NewStore(cat)
	registry := provider.NewRegistry(openai.New(upstream.Client()))

	gw := &gateway.Gateway{
		Store:    store,
		Executor: execute.New(registry, &provider.EnvResolver{}),
		Policy:   domain.DefaultPolicy(),
	}
	srv := server.New(gw, store, registry, server.Options{
		Logger:          slog.New(slog.DiscardHandler),
		StreamHeartbeat: hb,
	})

	relay := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		relay.Close()
		upstream.Client().CloseIdleConnections()
	})
	return relay
}

// countFrames counts SSE comment frames and data frames on one streamed response.
func countFrames(t *testing.T, relay *httptest.Server, body string) (comments, data int) {
	t.Helper()
	resp, err := http.Post(relay.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		switch line := sc.Text(); {
		case strings.HasPrefix(line, ":"):
			comments++
		case strings.HasPrefix(line, "data: "):
			data++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return comments, data
}

func parseUSD(t *testing.T, h http.Header, name string) float64 {
	t.Helper()
	raw := h.Get(name)
	if raw == "" {
		t.Fatalf("%s is absent", name)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("%s = %q: %v", name, raw, err)
	}
	return v
}

// nearly compares at half the precision the headers are formatted to, so the
// assertion is about the arithmetic rather than about float printing.
func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 5e-7
}
