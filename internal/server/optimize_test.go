package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/metrics"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/respcache"
	"github.com/Shashank-Panda/relay/internal/server"
	"github.com/Shashank-Panda/relay/internal/tenant"
)

// optCatalogYAML enables the response cache on relay/test and otherwise matches
// the catalog the Phase 2 tests use, so cost figures stay arithmetic: cheap is
// $1/$2 per 1M tokens, dear is $10/$20.
const optCatalogYAML = `
version: "test-opt-1"
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
      cached_input: 0.10
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
      cached_input: 1.00
      source: test
      verified_on: 2026-08-01
    quality: {coding: 0.90}
routes:
  - name: relay/test
    candidates: [openai/cheap@us, openai/dear@us]
    baseline: openai/dear@us
    weights: {cost: 1.0}
    fallback: openai/cheap@us
    cache:
      enabled: true
      ttl: 5m
  - name: relay/nocache
    candidates: [openai/cheap@us, openai/dear@us]
    baseline: openai/dear@us
    weights: {cost: 1.0}
`

const (
	leverKey = "sk-relay-test-levers"
	bareKey  = "sk-relay-test-bare"
)

// optTenantsYAML gives one tenant the recommended levers while staying in
// strict mode, and another nothing at all.
//
// That combination is the whole Phase 3 claim: a customer who has not granted
// permission to substitute any model still gets a measurable saving. It also
// pins the distinction the rest of these tests turn on — tenant strict mode
// governs substitution, not request rewriting.
func optTenantsYAML() string {
	return "default:\n  id: default\n  policy:\n    optimization_mode: strict\n" +
		"tenants:\n" +
		"  - id: levered\n    keys: [" + tenant.HashKey(leverKey) + "]\n" +
		"    policy:\n      optimization_mode: strict\n" +
		"      levers:\n        preset: recommended\n" +
		"  - id: bare\n    keys: [" + tenant.HashKey(bareKey) + "]\n" +
		"    policy:\n      optimization_mode: strict\n"
}

type optHarness struct {
	relay *httptest.Server
	admin *httptest.Server

	agg   *meter.Aggregator
	meter *meter.Meter
	cache *respcache.Store
	prom  *prometheus.Registry

	// upstreamCalls counts real provider contacts, which is how a cache hit is
	// distinguished from a fast miss. Asserting on a header alone would pass
	// even if the cache reported a hit and then called the provider anyway.
	upstreamCalls atomic.Int64

	// lastUpstream is the body the provider actually received, so a test can
	// check that a lever's effect reached the wire rather than only the log.
	lastUpstream atomic.Value // map[string]any
}

func newOptHarness(t *testing.T, respond http.HandlerFunc) *optHarness {
	t.Helper()
	t.Setenv("RELAY_CRED_OPENAI_PRIMARY", "sk-test")

	h := &optHarness{}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.upstreamCalls.Add(1)
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.lastUpstream.Store(body)
		}
		respond(w, r)
	}))
	t.Cleanup(upstream.Close)

	cat, err := catalog.Load(strings.NewReader(optCatalogYAML), catalog.Options{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, ep := range cat.Endpoints {
		ep.BaseURL = upstream.URL
	}
	store := catalog.NewStore(cat)

	tenants, err := tenant.Load(strings.NewReader(optTenantsYAML()))
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}

	promReg := prometheus.NewRegistry()
	mx := metrics.New(promReg)
	agg := meter.NewAggregator(time.Now().UTC())
	stats := meter.NewRouteStats(meter.DefaultMinSamples)
	mtr := meter.New(1024, agg, mx, stats)

	cache := respcache.New(respcache.Options{})
	client := upstream.Client()
	registry := provider.NewRegistry(openai.New(client))

	gw := &gateway.Gateway{
		Store:    store,
		Executor: execute.New(registry, &provider.EnvResolver{}),
		Tenants:  tenants,
		Policy:   domain.DefaultPolicy(),
		Stats:    stats,
		Cache:    cache,
	}

	srv := server.New(gw, store, registry, server.Options{
		Logger:  slog.New(slog.DiscardHandler),
		Tenants: tenants,
		Meter:   mtr,
		Metrics: mx,
	})

	h.relay = httptest.NewServer(srv.Handler())
	h.admin = httptest.NewServer(server.NewAdmin(agg, mtr, promReg).WithOptimizer(cache, stats).Handler())
	h.agg, h.meter, h.cache, h.prom = agg, mtr, cache, promReg

	t.Cleanup(func() {
		h.relay.Close()
		h.admin.Close()
		client.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mtr.Close(ctx)
	})
	return h
}

func (h *optHarness) post(t *testing.T, key, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.relay.URL+"/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
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

func (h *optHarness) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.meter.Written()+h.meter.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
}

func (h *optHarness) savings(t *testing.T) map[string]any {
	t.Helper()
	resp, err := http.Get(h.admin.URL + "/savings")
	if err != nil {
		t.Fatalf("GET /savings: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding report: %v", err)
	}
	return out
}

func jsonOK(usage string) string {
	return `{"id":"chatcmpl-up","model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"the answer"},"finish_reason":"stop"}],` +
		`"usage":` + usage + `}`
}

// longBody is big enough that the breakpoint lever's 1024-token minimum is
// comfortably cleared by the system prompt alone.
func longBody(model string, extra string) string {
	system := strings.Repeat("x", 8000)
	user := strings.Repeat("y", 8000)
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"system","content":%q},`+
		`{"role":"user","content":%q}]%s}`, model, system, user, extra)
}

// --- levers reach the wire ---

func TestLeversApplyInStrictMode(t *testing.T) {
	// The claim the whole phase rests on. This tenant has not granted permission
	// to substitute anything, and still gets an optimized request.
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":4000,"completion_tokens":100}`))
	})

	resp := h.post(t, leverKey, longBody("relay/test", ""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Nothing was substituted: strict mode still holds.
	if got := resp.Header.Get(server.HeaderSubstituted); got != "" {
		t.Errorf("%s = %q on a strict-mode request", server.HeaderSubstituted, got)
	}

	// And the optimization is disclosed, because an optimization the customer
	// cannot see is indistinguishable from a bug.
	ops := resp.Header.Get(server.HeaderOptimizations)
	if !strings.Contains(ops, domain.LeverCacheBreakpoints) {
		t.Fatalf("%s = %q, want it to name %s",
			server.HeaderOptimizations, ops, domain.LeverCacheBreakpoints)
	}
}

func TestNoLeversForATenantWhoEnabledNone(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":4000,"completion_tokens":100}`))
	})

	resp := h.post(t, bareKey, longBody("relay/test", ""), nil)

	// The zero LeverConfig enables nothing. A tenant must never discover their
	// requests are being rewritten because a field was left blank.
	if got := resp.Header.Get(server.HeaderOptimizations); got != "" {
		t.Errorf("%s = %q for a tenant with no levers configured",
			server.HeaderOptimizations, got)
	}
}

func TestPinStrictDisablesEverything(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":4000,"completion_tokens":100}`))
	})

	// The per-request escape hatch is stronger than the tenant's mode: it is the
	// one request where nothing may be touched, and an escape hatch with
	// exceptions is not one.
	resp := h.post(t, leverKey, longBody("relay/test", ""),
		map[string]string{server.HeaderPin: "strict"})

	if got := resp.Header.Get(server.HeaderOptimizations); got != "" {
		t.Errorf("%s = %q under X-Relay-Pin: strict", server.HeaderOptimizations, got)
	}
	if cache := resp.Header.Get(server.HeaderCache); !strings.HasPrefix(cache, "off") {
		t.Errorf("%s = %q under X-Relay-Pin: strict, want the cache off", server.HeaderCache, cache)
	}
}

// --- response cache ---

func TestCacheHitAvoidsTheProvider(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":100000,"completion_tokens":10000}`))
	})

	body := longBody("relay/test", "")

	first := h.post(t, leverKey, body, nil)
	if got := first.Header.Get(server.HeaderCache); got != "miss" {
		t.Fatalf("first %s = %q, want miss", server.HeaderCache, got)
	}
	drainBody(t, first)

	second := h.post(t, leverKey, body, nil)
	if got := second.Header.Get(server.HeaderCache); got != "hit" {
		t.Fatalf("second %s = %q, want hit", server.HeaderCache, got)
	}

	if n := h.upstreamCalls.Load(); n != 1 {
		t.Errorf("provider was called %d times for two identical requests; a cache "+
			"that reports a hit and calls anyway is worse than no cache", n)
	}

	// The reply is the same answer, not an empty shell.
	var out map[string]any
	if err := json.NewDecoder(second.Body).Decode(&out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("cached response has %d choices", len(choices))
	}
}

func TestCacheIsScopedToTheTenant(t *testing.T) {
	// The failure this is here to prevent is a data breach with a nice
	// performance graph.
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":4000,"completion_tokens":100}`))
	})

	body := longBody("relay/test", "")
	drainBody(t, h.post(t, leverKey, body, nil))

	other := h.post(t, bareKey, body, nil)
	if got := other.Header.Get(server.HeaderCache); got == "hit" {
		t.Fatal("one tenant was served another tenant's cached answer")
	}
	if n := h.upstreamCalls.Load(); n != 2 {
		t.Errorf("provider called %d times, want 2: the second tenant must get a live call", n)
	}
}

func TestCacheIsSkippedForANonZeroTemperature(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":4000,"completion_tokens":100}`))
	})

	body := longBody("relay/test", `,"temperature":0.9`)
	drainBody(t, h.post(t, leverKey, body, nil))
	second := h.post(t, leverKey, body, nil)

	// A caller who set a temperature asked for variation. Returning the same
	// answer every time is not a cheaper version of that request.
	if got := second.Header.Get(server.HeaderCache); got == "hit" {
		t.Error("cached a request that asked for a different answer each time")
	}
	if n := h.upstreamCalls.Load(); n != 2 {
		t.Errorf("provider called %d times, want 2", n)
	}
}

func TestCacheIsOffForARouteThatDidNotOptIn(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":4000,"completion_tokens":100}`))
	})

	body := longBody("relay/nocache", "")
	drainBody(t, h.post(t, leverKey, body, nil))
	second := h.post(t, leverKey, body, nil)

	if got := second.Header.Get(server.HeaderCache); !strings.HasPrefix(got, "off") {
		t.Errorf("%s = %q for a route that did not enable the cache", server.HeaderCache, got)
	}
	if n := h.upstreamCalls.Load(); n != 2 {
		t.Errorf("provider called %d times, want 2", n)
	}
}

func TestNoCacheHeaderBypassesBothDirections(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":4000,"completion_tokens":100}`))
	})

	body := longBody("relay/test", "")

	// A bypassed request must not populate the cache either, or a caller asking
	// for a fresh answer would decide what everybody else is served.
	drainBody(t, h.post(t, leverKey, body, map[string]string{server.HeaderNoCache: "true"}))

	second := h.post(t, leverKey, body, nil)
	if got := second.Header.Get(server.HeaderCache); got == "hit" {
		t.Error("a bypassed request wrote to the cache")
	}
}

func TestCachedStreamIsReplayedAsAStream(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"id":"c","object":"chat.completion.chunk","choices":`+
			`[{"index":0,"delta":{"content":"streamed"},"finish_reason":"stop"}]}`+"\n\n"+
			"data: "+`{"id":"c","object":"chat.completion.chunk","choices":[],`+
			`"usage":{"prompt_tokens":50000,"completion_tokens":5000}}`+"\n\n"+
			"data: [DONE]\n\n")
	})

	body := longBody("relay/test", `,"stream":true`)

	first := h.post(t, leverKey, body, nil)
	firstText := readSSE(t, first)
	if !strings.Contains(firstText, "streamed") {
		t.Fatalf("live stream did not carry the content: %q", firstText)
	}

	second := h.post(t, leverKey, body, nil)
	if got := second.Header.Get(server.HeaderCache); got != "hit" {
		t.Fatalf("second %s = %q, want hit", server.HeaderCache, got)
	}
	if ct := second.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q; a cached answer must come back in the shape the "+
			"caller asked for", ct)
	}

	replayed := readSSE(t, second)
	if !strings.Contains(replayed, "streamed") {
		t.Errorf("replayed stream lost the content: %q", replayed)
	}
	if !strings.Contains(replayed, "[DONE]") {
		t.Error("replayed stream has no terminator; every OpenAI SDK waits for one")
	}
	if n := h.upstreamCalls.Load(); n != 1 {
		t.Errorf("provider called %d times for two identical streams", n)
	}
}

func TestCancelledStreamIsNotCached(t *testing.T) {
	// A partial answer in a response cache is the worst possible entry: it looks
	// complete to everyone who reads it afterwards.
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: "+`{"id":"c","object":"chat.completion.chunk","choices":`+
			`[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// Never completes. The client below gives up first.
		<-r.Context().Done()
	})

	body := longBody("relay/test", `,"stream":true`)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		h.relay.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+leverKey)

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	if st := h.cache.Stats(); st.Stores != 0 {
		t.Errorf("stored %d entries from a stream that never finished", st.Stores)
	}
}

// --- savings ---

func TestCacheHitIsAMeasuredSavingInStrictMode(t *testing.T) {
	// Phase 3's "done when", stated as a test: a tenant in strict mode shows a
	// positive *measured* saving — not a shadow figure, not a counterfactual.
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":100000,"completion_tokens":10000}`))
	})

	body := longBody("relay/test", "")
	drainBody(t, h.post(t, leverKey, body, nil))

	hit := h.post(t, leverKey, body, nil)
	if got := hit.Header.Get(server.HeaderCache); got != "hit" {
		t.Fatalf("%s = %q, want hit", server.HeaderCache, got)
	}
	// 100k input at $10/1M plus 10k output at $20/1M = $1.00 + $0.20.
	if got := hit.Header.Get(server.HeaderSaved); got != "1.200000" {
		t.Errorf("%s = %q, want 1.200000: a cache hit saves the whole baseline cost, "+
			"because nothing was bought", server.HeaderSaved, got)
	}
	if got := hit.Header.Get(server.HeaderSavedEstimated); got != "" {
		t.Errorf("%s = %q; a cache hit knows its exact token counts", server.HeaderSavedEstimated, got)
	}

	h.settle(t)
	report := h.savings(t)
	overall, _ := report["overall"].(map[string]any)

	if got := overall["cache_hits"]; got != float64(1) {
		t.Errorf("cache_hits = %v, want 1", got)
	}
	if got := overall["saved_micros"]; got != float64(1_200_000) {
		t.Errorf("saved_micros = %v, want 1200000", got)
	}
	// Money saved, not money that could have been. The shadow figure stays zero
	// because nothing was substituted, and the two must never be summable.
	if got := overall["shadow_saved_micros"]; got != float64(0) {
		t.Errorf("shadow_saved_micros = %v, want 0", got)
	}
	// The tokens avoided are reported separately from the tokens bought. Adding
	// them would inflate every per-token figure derived from the totals.
	if got := overall["cache_hit_tokens_avoided"]; got != float64(110_000) {
		t.Errorf("cache_hit_tokens_avoided = %v, want 110000", got)
	}
	if got := overall["input_tokens"]; got != float64(100_000) {
		t.Errorf("input_tokens = %v, want 100000 — only the purchased request counts", got)
	}
}

func TestBreakpointsAreReportedAgainstProviderCacheReads(t *testing.T) {
	// ADR-0008's specific warning: an incorrectly placed breakpoint is silently
	// useless, so the inserted count must be read against what the provider said
	// it actually cached. The report has to make that comparison available and
	// say so when it fails.
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Markers placed, nothing cached: exactly the silent failure.
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":100000,"completion_tokens":1000}`))
	})

	drainBody(t, h.post(t, leverKey, longBody("relay/test", ""), nil))
	h.settle(t)

	report := h.savings(t)
	overall, _ := report["overall"].(map[string]any)

	if got, _ := overall["breakpoints_inserted"].(float64); got <= 0 {
		t.Fatalf("breakpoints_inserted = %v, want above zero", overall["breakpoints_inserted"])
	}
	if got := overall["cached_input_tokens"]; got != float64(0) {
		t.Fatalf("cached_input_tokens = %v, want 0 for this fixture", got)
	}
	note, _ := report["note"].(string)
	if !strings.Contains(note, "cached none of them") {
		t.Errorf("note = %q; a report that lets inserted markers read as evidence of "+
			"savings is the failure ADR-0008 names", note)
	}
}

func TestCachedInputShareIsReported(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(
			`{"prompt_tokens":100000,"completion_tokens":1000,`+
				`"prompt_tokens_details":{"cached_tokens":80000}}`))
	})

	drainBody(t, h.post(t, leverKey, longBody("relay/test", ""), nil))
	h.settle(t)

	report := h.savings(t)
	if got := report["cached_input_share"]; got != 0.8 {
		t.Errorf("cached_input_share = %v, want 0.8 — this is the number that says "+
			"whether breakpoint insertion worked", got)
	}
}

// --- dry run ---

func TestDryRunContactsNoProvider(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("dry run called the provider")
	})

	resp := h.post(t, leverKey, longBody("relay/test", ""),
		map[string]string{server.HeaderDryRun: "1"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if out["object"] != "relay.dry_run" {
		t.Errorf("object = %v", out["object"])
	}
	if out["chosen"] == "" || out["chosen"] == nil {
		t.Error("dry run did not report a chosen endpoint")
	}

	ops, _ := out["optimizations"].([]any)
	if len(ops) == 0 {
		t.Fatal("dry run reported no optimizations for a levered tenant")
	}
	first, _ := ops[0].(map[string]any)
	// before/after/reason, not just a name. This is the document that answers
	// "why is this answer shorter than yesterday's".
	for _, field := range []string{"lever", "before", "after", "reason"} {
		if _, ok := first[field]; !ok {
			t.Errorf("optimization is missing %q: %v", field, first)
		}
	}

	optimizer, _ := out["optimizer"].(map[string]any)
	if optimizer["outcome"] != "applied" {
		t.Errorf("optimizer.outcome = %v, want applied", optimizer["outcome"])
	}
	if optimizer["budget_us"] != float64(3000) {
		t.Errorf("optimizer.budget_us = %v, want 3000", optimizer["budget_us"])
	}

	cache, _ := out["cache"].(map[string]any)
	if cache["eligible"] != true {
		t.Errorf("cache.eligible = %v, want true on a cache-enabled route", cache["eligible"])
	}

	// Nothing was recorded, because nothing happened.
	h.settle(t)
	if n := h.meter.Written(); n != 0 {
		t.Errorf("dry run wrote %d ledger records", n)
	}
}

func TestDryRunExplainsWhyTheCacheIsOff(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("dry run called the provider")
	})

	resp := h.post(t, leverKey, longBody("relay/nocache", ""),
		map[string]string{server.HeaderDryRun: "yes"})

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	cache, _ := out["cache"].(map[string]any)
	if cache["eligible"] != false {
		t.Fatalf("cache.eligible = %v, want false", cache["eligible"])
	}
	if reason, _ := cache["skipped_because"].(string); reason == "" {
		t.Error("no reason given; \"the cache is off\" without a reason is not actionable")
	}
}

// --- metrics ---

func TestOptimizerMetricsArePublished(t *testing.T) {
	h := newOptHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonOK(`{"prompt_tokens":4000,"completion_tokens":100}`))
	})

	body := longBody("relay/test", "")
	drainBody(t, h.post(t, leverKey, body, nil))
	drainBody(t, h.post(t, leverKey, body, nil)) // hit
	h.settle(t)

	families, err := h.prom.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}

	for _, want := range []string{
		"relay_optimizations_total",
		"relay_cache_breakpoints_inserted_total",
		"relay_optimize_duration_seconds",
		"relay_cache_hits_total",
		"relay_cache_misses_total",
	} {
		if !seen[want] {
			t.Errorf("%s was never published", want)
		}
	}
}

// --- helpers ---

func drainBody(t *testing.T, resp *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func readSSE(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}
	_ = resp.Body.Close()
	return string(buf)
}
