package server_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/server"
)

// credCatalog has two endpoints behind two different credential refs, one
// cheaper than the other. Both are served by the same mock, so the only thing
// that can distinguish them is the credential.
const credCatalog = `
version: "cred-test"
endpoints:
  - id: p/dear@r
    provider: openai
    model: dear
    deployment: r
    credential_ref: have-this-one
    base_url: BASE/dear
    capabilities: {streaming: true, tools: true}
    limits: {context_window: 100000, max_output_tokens: 4096}
    pricing: {input: 10.00, output: 30.00, source: test, verified_on: 2026-08-01}
    quality: {coding: 0.90}
  - id: p/cheap@r
    provider: openai
    model: cheap
    deployment: r
    credential_ref: do-not-have-this-one
    base_url: BASE/cheap
    capabilities: {streaming: true, tools: true}
    limits: {context_window: 100000, max_output_tokens: 4096}
    pricing: {input: 1.00, output: 2.00, source: test, verified_on: 2026-08-01}
    quality: {coding: 0.70}
routes:
  - name: relay/cred
    candidates: [p/dear@r, p/cheap@r]
    baseline: p/dear@r
    weights: {cost: 0.60, quality.coding: 0.40}
    max_attempts: 3
aliases:
  cred: p/dear@r
`

type credHarness struct {
	relay *httptest.Server

	mu   sync.Mutex
	hits map[string]int
}

func newCredHarness(t *testing.T) *credHarness {
	t.Helper()
	t.Setenv("RELAY_CRED_HAVE_THIS_ONE", "sk-test")
	// do-not-have-this-one is deliberately unset.

	h := &credHarness{hits: map[string]int{}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site := "dear"
		if strings.Contains(r.URL.Path, "/cheap") {
			site = "cheap"
		}
		h.mu.Lock()
		h.hits[site]++
		h.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
	}))
	t.Cleanup(upstream.Close)

	cat, err := catalog.Load(
		strings.NewReader(strings.ReplaceAll(credCatalog, "BASE", upstream.URL)),
		catalog.Options{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store := catalog.NewStore(cat)
	registry := provider.NewRegistry(openai.New(upstream.Client()))
	resolver := &provider.EnvResolver{}

	pol := domain.DefaultPolicy()
	pol.OptimizationMode = domain.ModeOptimize

	gw := &gateway.Gateway{
		Store:       store,
		Executor:    execute.New(registry, resolver),
		Credentials: resolver,
		Policy:      pol,
	}
	srv := server.New(gw, store, registry, server.Options{Logger: slog.New(slog.DiscardHandler)})

	h.relay = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		h.relay.Close()
		upstream.Client().CloseIdleConnections()
	})
	return h
}

func (h *credHarness) post(t *testing.T, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.relay.URL+"/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *credHarness) hit(site string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[site]
}

const credBody = `{"model":"relay/cred","messages":[{"role":"user","content":"hi"}]}`

// TestUnavailableCredentialIsNeverAttempted asserts on the calls made, not on
// the answer returned.
//
// The bug being fixed is wasted attempts, and an assertion about the final
// response would pass for an implementation that still called the endpoint it
// has no key for, failed, and recovered — which is the behaviour this change
// exists to remove. Each such attempt consumes a slot from the request's attempt
// budget, so the cost lands on the *next* real failure, where it is invisible.
func TestUnavailableCredentialIsNeverAttempted(t *testing.T) {
	h := newCredHarness(t)

	// The cheap endpoint would win on score — it is ten times cheaper and the
	// cost weight dominates. It must not be selected, because its credential
	// is not configured.
	resp := h.post(t, credBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	drainBody(t, resp)

	if got := resp.Header.Get(server.HeaderEndpoint); got != "p/dear@r" {
		t.Errorf("served %q, want the endpoint whose credential exists", got)
	}
	if n := h.hit("cheap"); n != 0 {
		t.Errorf("the unauthenticatable endpoint was called %d times; routing is "+
			"supposed to eliminate it, not let the executor discover it", n)
	}
	if n := h.hit("dear"); n != 1 {
		t.Errorf("the usable endpoint was called %d times, want 1", n)
	}
	if got := resp.Header.Get(server.HeaderAttempts); got != "" && got != "1" {
		t.Errorf("%s = %q, want a single attempt", server.HeaderAttempts, got)
	}
}

// TestDryRunReportsWhichCredentialsAreMissing is what the console turns into
// "paste a key for these to include them", so the UI never hardcodes a provider
// name and follows the catalog automatically.
func TestDryRunReportsWhichCredentialsAreMissing(t *testing.T) {
	h := newCredHarness(t)

	resp := h.post(t, credBody, map[string]string{server.HeaderDryRun: "1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var got struct {
		Chosen      string `json:"chosen"`
		Credentials struct {
			Assumed   bool     `json:"assumed"`
			Available []string `json:"available"`
			Missing   []string `json:"missing"`
		} `json:"credentials"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Credentials.Assumed {
		t.Error("assumed = true without the header")
	}
	if !reflectEqualStrings(got.Credentials.Available, []string{"have-this-one"}) {
		t.Errorf("available = %v, want [have-this-one]", got.Credentials.Available)
	}
	if !reflectEqualStrings(got.Credentials.Missing, []string{"do-not-have-this-one"}) {
		t.Errorf("missing = %v, want [do-not-have-this-one]", got.Credentials.Missing)
	}
}

// TestAssumeCredentialsRestoresTheFullRanking is the conflict this header
// exists to resolve.
//
// Credential filtering and a keyless explanation destroy each other: the moment
// routing eliminates endpoints for want of a key, a dry run on a fresh clone
// collapses from a full ranking to whatever the one free endpoint is, and the
// most useful thing this product can show a prospective user stops working.
func TestAssumeCredentialsRestoresTheFullRanking(t *testing.T) {
	h := newCredHarness(t)

	plain := decodeRanked(t, h.post(t, credBody, map[string]string{
		server.HeaderDryRun: "1",
	}))
	assumed := decodeRanked(t, h.post(t, credBody, map[string]string{
		server.HeaderDryRun:            "1",
		server.HeaderAssumeCredentials: "all",
	}))

	if len(assumed.Ranked) <= len(plain.Ranked) {
		t.Fatalf("assumed ranked %d, plain ranked %d; the header is doing nothing",
			len(assumed.Ranked), len(plain.Ranked))
	}
	if !assumed.Credentials.Assumed {
		t.Error("assumed flag is not set; the reader would not know the ranking " +
			"was computed over keys they do not hold")
	}
	// Still honest about what the reader actually has.
	if !reflectEqualStrings(assumed.Credentials.Missing, []string{"do-not-have-this-one"}) {
		t.Errorf("missing = %v under assume; the ranking may be hypothetical but "+
			"the credential report must not be", assumed.Credentials.Missing)
	}
}

// TestAssumeCredentialsIsIgnoredOnALiveRequest is the safety half.
//
// A header that could make a live request route to an endpoint Relay cannot
// authenticate to would be a denial of service with a polite name — and it
// would be reachable by anyone who could send a request.
func TestAssumeCredentialsIsIgnoredOnALiveRequest(t *testing.T) {
	h := newCredHarness(t)

	resp := h.post(t, credBody, map[string]string{server.HeaderAssumeCredentials: "all"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	drainBody(t, resp)

	if got := resp.Header.Get(server.HeaderEndpoint); got != "p/dear@r" {
		t.Errorf("served %q; the header must not influence a live decision", got)
	}
	if n := h.hit("cheap"); n != 0 {
		t.Errorf("the unauthenticatable endpoint was called %d times on a live "+
			"request carrying X-Relay-Assume-Credentials", n)
	}
}

// --- helpers ---

type rankedView struct {
	Chosen string `json:"chosen"`
	Ranked []struct {
		Endpoint string `json:"endpoint"`
	} `json:"ranked"`
	Credentials struct {
		Assumed   bool     `json:"assumed"`
		Available []string `json:"available"`
		Missing   []string `json:"missing"`
	} `json:"credentials"`
}

func decodeRanked(t *testing.T, resp *http.Response) rankedView {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var v rankedView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func reflectEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
