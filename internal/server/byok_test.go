package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/respcache"
	"github.com/Shashank-Panda/relay/internal/server"
)

// byokCatalog is one endpoint whose credential is deliberately absent from the
// environment, and a route with the response cache enabled. Everything below is
// about which key reaches the provider and which answers may be shared.
const byokCatalog = `
version: "byok-test"
endpoints:
  - id: p/m@d
    provider: openai
    model: m
    deployment: d
    credential_ref: openai-primary
    base_url: BASE
    capabilities: {streaming: true, tools: true}
    limits: {context_window: 100000, max_output_tokens: 4096}
    pricing: {input: 1.00, output: 2.00, source: test, verified_on: 2026-08-01}
    quality: {coding: 0.80}
routes:
  - name: relay/byok
    candidates: [p/m@d]
    baseline: p/m@d
    weights: {cost: 1.0}
    cache: {enabled: true, ttl: 5m}
aliases:
  m: p/m@d
`

type byokHarness struct {
	relay *httptest.Server
	logs  *bytes.Buffer

	mu      sync.Mutex
	keys    []string
	bodies  []string
	records []meter.Record
}

func newBYOKHarness(t *testing.T) *byokHarness {
	t.Helper()
	// openai-primary is deliberately NOT in the environment: every credential
	// below has to have come from the request.

	h := &byokHarness{logs: &bytes.Buffer{}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.keys = append(h.keys, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		h.bodies = append(h.bodies, string(body))
		h.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
	}))
	t.Cleanup(upstream.Close)

	cat, err := catalog.Load(
		strings.NewReader(strings.ReplaceAll(byokCatalog, "BASE", upstream.URL)),
		catalog.Options{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store := catalog.NewStore(cat)
	registry := provider.NewRegistry(openai.New(upstream.Client()))
	resolver := &provider.EnvResolver{}

	capture := meter.SinkFunc(func(r meter.Record) {
		h.mu.Lock()
		h.records = append(h.records, r)
		h.mu.Unlock()
	})
	mtr := meter.New(256, capture)

	gw := &gateway.Gateway{
		Store:       store,
		Executor:    execute.New(registry, resolver),
		Credentials: resolver,
		Cache:       respcache.New(respcache.Options{MaxEntries: 64, MaxBytes: 1 << 20}),
		Policy:      domain.DefaultPolicy(),
	}
	srv := server.New(gw, store, registry, server.Options{
		// A real handler, into a buffer, so the leak assertions below inspect
		// what would actually have been written rather than trusting that
		// nothing writes.
		Logger:                   slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Meter:                    mtr,
		AllowInsecureCredentials: true,
	})

	h.relay = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		h.relay.Close()
		upstream.Client().CloseIdleConnections()
		// The meter runs a goroutine, and the streaming tests in this package
		// assert with goleak that none survive. A harness that starts one and
		// walks away fails a test it has nothing to do with.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mtr.Close(ctx)
	})
	return h
}

func (h *byokHarness) post(t *testing.T, body string, headers ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.relay.URL+"/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *byokHarness) upstreamKeys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.keys...)
}

const byokBody = `{"model":"relay/byok","messages":[{"role":"user","content":"the same question"}]}`

// TestRequestCredentialReachesTheProvider is the basic claim: a key sent on the
// request is the key the provider is called with, with nothing configured
// server-side.
func TestRequestCredentialReachesTheProvider(t *testing.T) {
	h := newBYOKHarness(t)

	resp := h.post(t, byokBody, server.HeaderCredential, "openai-primary sk-alice")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	drainBody(t, resp)

	keys := h.upstreamKeys()
	if len(keys) != 1 || keys[0] != "sk-alice" {
		t.Fatalf("provider was called with %v, want [sk-alice]", keys)
	}
}

// TestTwoBYOKPrincipalsUnderOneTenantDoNotShareACacheEntry is the
// security-critical one.
//
// Both callers are the anonymous default tenant, because that is what everyone
// is on a hosted deployment before signing up. They send byte-identical
// requests. Under tenant-scoped caching that is one key and a hit — so the
// second caller receives an answer generated on the first caller's credential,
// together with confirmation that a stranger asked that exact question.
//
// The failure would be completely silent: a 200, a plausible answer, and a
// pleasing cache-hit graph.
func TestTwoBYOKPrincipalsUnderOneTenantDoNotShareACacheEntry(t *testing.T) {
	h := newBYOKHarness(t)

	first := h.post(t, byokBody, server.HeaderCredential, "openai-primary sk-alice")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	drainBody(t, first)

	second := h.post(t, byokBody, server.HeaderCredential, "openai-primary sk-bob")
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d", second.StatusCode)
	}
	if got := second.Header.Get(server.HeaderCache); strings.HasPrefix(got, "hit") {
		t.Errorf("%s = %q: Bob was served an answer generated on Alice's key",
			server.HeaderCache, got)
	}
	drainBody(t, second)

	keys := h.upstreamKeys()
	if len(keys) != 2 {
		t.Fatalf("provider called %d times with %v, want one call per principal", len(keys), keys)
	}
	if keys[0] == keys[1] {
		t.Errorf("both calls used %q", keys[0])
	}
}

// TestSameBYOKPrincipalStillHitsTheCache is the other half, and the reason the
// cache is scoped rather than simply switched off under BYOK.
//
// The response cache is one of the four levers this product sells and the only
// one that returns the entire cost on a hit. Disabling it whenever a caller
// brings their own key would remove a headline saving from precisely the path
// that is meant to demonstrate the product.
func TestSameBYOKPrincipalStillHitsTheCache(t *testing.T) {
	h := newBYOKHarness(t)

	for i := 0; i < 2; i++ {
		resp := h.post(t, byokBody, server.HeaderCredential, "openai-primary sk-alice")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if i == 1 && !strings.HasPrefix(resp.Header.Get(server.HeaderCache), "hit") {
			t.Errorf("%s = %q on an identical repeat from the same principal",
				server.HeaderCache, resp.Header.Get(server.HeaderCache))
		}
		drainBody(t, resp)
	}

	if n := len(h.upstreamKeys()); n != 1 {
		t.Errorf("provider called %d times; the second should have been served from cache", n)
	}
}

// TestCredentialNeverEscapes is the property that makes accepting a stranger's
// key defensible at all.
//
// Asserted against four sinks at once — the structured log, every ledger record,
// the response headers and body, and Go's own reflection-based formatting —
// because each is a different way the same string escapes, and each has a
// different reason nobody would notice.
func TestCredentialNeverEscapes(t *testing.T) {
	const canary = "sk-CANARY-9f3b2a71c4e5"

	h := newBYOKHarness(t)
	resp := h.post(t, byokBody, server.HeaderCredential, "openai-primary "+canary)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var hdr bytes.Buffer
	_ = resp.Header.Write(&hdr)

	h.mu.Lock()
	recs := append([]meter.Record(nil), h.records...)
	logs := h.logs.String()
	h.mu.Unlock()

	ledger, err := json.Marshal(recs)
	if err != nil {
		t.Fatalf("marshal records: %v", err)
	}

	for _, sink := range []struct{ name, content string }{
		{"structured log", logs},
		{"savings ledger", string(ledger)},
		{"response headers", hdr.String()},
		{"response body", string(body)},
	} {
		if strings.Contains(sink.content, canary) {
			t.Errorf("the credential appears in the %s", sink.name)
		}
		// Also refuse a prefix. A key truncated "for safety" is still enough to
		// identify an account and is often enough to correlate a leak.
		if strings.Contains(sink.content, canary[:12]) {
			t.Errorf("a prefix of the credential appears in the %s", sink.name)
		}
	}
}

// TestKeySetRedactsUnderEveryFormatVerb guards the container the same way
// Credential.String already guards its contents.
//
// Without it, a single %v of an execute.Request — in a log line, a wrapped
// error, or a panic dump — renders a map of live provider keys.
func TestKeySetRedactsUnderEveryFormatVerb(t *testing.T) {
	k := provider.NewKeySet(map[string]string{
		"openai-primary":    "sk-CANARY-openai",
		"anthropic-primary": "sk-CANARY-anthropic",
	})

	for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
		got := fmt.Sprintf(format, k)
		if strings.Contains(got, "CANARY") {
			t.Errorf("%s printed a secret: %s", format, got)
		}
		if !strings.Contains(got, "openai-primary") {
			t.Errorf("%s = %q; the refs are safe to print and are what makes the "+
				"output useful", format, got)
		}
	}
}

// TestMalformedCredentialHeaderIsRejectedWithoutEchoing checks the error path,
// which is where a credential most reliably ends up in somebody's issue tracker.
func TestMalformedCredentialHeaderIsRejectedWithoutEchoing(t *testing.T) {
	h := newBYOKHarness(t)

	for _, bad := range []string{
		"sk-CANARY-no-ref-at-all",
		"openai-primary ",
		" sk-CANARY-leading-space-only",
	} {
		resp := h.post(t, byokBody, server.HeaderCredential, bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d for %q, want 400", resp.StatusCode, bad)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "CANARY") {
			t.Errorf("the error body echoed the value: %s", body)
		}
	}
}

// TestDuplicateCredentialRefIsRefused.
//
// Silently picking one means a caller who rotated a key mid-script cannot tell
// which one was used, and the request that reveals it is the one that failed.
func TestDuplicateCredentialRefIsRefused(t *testing.T) {
	h := newBYOKHarness(t)
	resp := h.post(t, byokBody,
		server.HeaderCredential, "openai-primary sk-one",
		server.HeaderCredential, "openai-primary sk-two")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	drainBody(t, resp)
}

// TestInsecureCredentialIsRefusedByDefault.
//
// Refused rather than warned about: the request would otherwise succeed, so a
// key crossing the network in the clear would leave no trace anywhere.
func TestInsecureCredentialIsRefusedByDefault(t *testing.T) {
	t.Setenv("RELAY_CRED_OPENAI_PRIMARY", "")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[]}`)
	}))
	t.Cleanup(upstream.Close)

	cat, err := catalog.Load(
		strings.NewReader(strings.ReplaceAll(byokCatalog, "BASE", upstream.URL)),
		catalog.Options{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store := catalog.NewStore(cat)
	registry := provider.NewRegistry(openai.New(upstream.Client()))
	resolver := &provider.EnvResolver{}

	// AllowInsecureCredentials left at its zero value, which is the point.
	srv := server.New(&gateway.Gateway{
		Store:       store,
		Executor:    execute.New(registry, resolver),
		Credentials: resolver,
		Policy:      domain.DefaultPolicy(),
	}, store, registry, server.Options{Logger: slog.New(slog.DiscardHandler)})

	relay := httptest.NewServer(srv.Handler())
	t.Cleanup(relay.Close)

	req, _ := http.NewRequest(http.MethodPost, relay.URL+"/v1/chat/completions",
		strings.NewReader(byokBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(server.HeaderCredential, "openai-primary sk-CANARY-plaintext")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d over plaintext http, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "CANARY") {
		t.Error("the refusal echoed the key it was refusing")
	}
}
