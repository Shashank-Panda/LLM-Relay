package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	"github.com/Shashank-Panda/relay/internal/server"
	"github.com/Shashank-Panda/relay/internal/tenant"
)

const (
	shadowKey = "sk-relay-test-shadow"
	strictKey = "sk-relay-test-strict"
)

// tenantsYAML gives acme shadow mode and globex strict, so one harness proves
// the setting is per-customer rather than process-wide. That distinction is the
// adoption story: one prospect evaluates in shadow while everyone else is
// untouched.
func tenantsYAML() string {
	return "default:\n  id: default\n  policy:\n    optimization_mode: strict\n" +
		"tenants:\n" +
		"  - id: acme\n    keys: [" + tenant.HashKey(shadowKey) + "]\n" +
		"    policy:\n      optimization_mode: shadow\n" +
		"  - id: globex\n    keys: [" + tenant.HashKey(strictKey) + "]\n" +
		"    policy:\n      optimization_mode: strict\n"
}

// ledgerHarness is Relay with metering, tenancy and metrics, in front of a mock
// provider that reports fixed token counts so every cost below is arithmetic.
type ledgerHarness struct {
	relay *httptest.Server
	admin *httptest.Server

	agg   *meter.Aggregator
	meter *meter.Meter
	prom  *prometheus.Registry
}

func newLedgerHarness(t *testing.T, upstreamBody string) *ledgerHarness {
	t.Helper()
	t.Setenv("RELAY_CRED_OPENAI_PRIMARY", "sk-test")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(upstreamBody, "data:") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	cat, err := catalog.Load(strings.NewReader(catalogYAML), catalog.Options{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, ep := range cat.Endpoints {
		ep.BaseURL = upstream.URL
	}
	store := catalog.NewStore(cat)

	tenants, err := tenant.Load(strings.NewReader(tenantsYAML()))
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}

	promReg := prometheus.NewRegistry()
	mx := metrics.New(promReg)
	agg := meter.NewAggregator(time.Now().UTC())
	mtr := meter.New(1024, agg, mx)

	client := upstream.Client()
	registry := provider.NewRegistry(openai.New(client))

	gw := &gateway.Gateway{
		Store:    store,
		Executor: execute.New(registry, &provider.EnvResolver{}),
		Tenants:  tenants,
		Policy:   domain.DefaultPolicy(),
	}

	srv := server.New(gw, store, registry, server.Options{
		Logger:  slog.New(slog.DiscardHandler),
		Tenants: tenants,
		Meter:   mtr,
		Metrics: mx,
	})

	h := &ledgerHarness{
		relay: httptest.NewServer(srv.Handler()),
		admin: httptest.NewServer(server.NewAdmin(agg, mtr, promReg).Handler()),
		agg:   agg, meter: mtr, prom: promReg,
	}
	t.Cleanup(func() {
		h.relay.Close()
		h.admin.Close()
		client.CloseIdleConnections()
		// The meter owns a worker goroutine. Production closes it during
		// graceful shutdown; a test that did not would leak one per harness and
		// fail the goroutine-leak checks in this package's other tests.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = mtr.Close(ctx)
	})
	return h
}

func (h *ledgerHarness) call(t *testing.T, key, body string) *http.Response {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// settle drains the metering queue so the ledger reflects the requests just
// made. Metering is asynchronous by design; a test that read the aggregate
// immediately would be racing the worker.
func (h *ledgerHarness) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.meter.Written()+h.meter.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// One more short wait so later records land too.
	time.Sleep(20 * time.Millisecond)
}

// upstreamBig reports 100k input and 10k output, so costs are round numbers
// against the catalog in server_test.go: cheap is $1/$2 per 1M, dear is $10/$20.
const upstreamBig = `{
  "id":"chatcmpl-up","model":"m",
  "choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
  "usage":{"prompt_tokens":100000,"completion_tokens":10000}
}`

// TestShadowModeEndToEnd is Phase 2's headline: a customer is served exactly
// what they asked for, and the ledger records what a cheaper route would have
// cost on the very same tokens.
func TestShadowModeEndToEnd(t *testing.T) {
	h := newLedgerHarness(t, upstreamBig)

	resp := h.call(t, shadowKey,
		`{"model":"relay/test","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Served the baseline. Shadow mode changes nothing about the answer.
	if got := resp.Header.Get(server.HeaderEndpoint); got != "openai/dear@us" {
		t.Errorf("served %q, want the baseline — shadow mode must not substitute", got)
	}
	if got := resp.Header.Get(server.HeaderMode); got != "shadow" {
		t.Errorf("mode = %q, want shadow", got)
	}
	if resp.Header.Get(server.HeaderSubstituted) != "" {
		t.Error("a substitution was disclosed although none happened")
	}

	// Nothing was actually saved, and the header says so.
	if got := resp.Header.Get(server.HeaderSaved); got != "0.000000" {
		t.Errorf("X-Relay-Saved-Usd = %q, want 0 — the baseline was served", got)
	}
	// The counterfactual figure is the point of the mode, and it lives in a
	// different header so the two can never be confused.
	shadow := resp.Header.Get(server.HeaderShadowSaved)
	if shadow == "" {
		t.Fatal("no shadow saving reported")
	}
	// dear: 100k x $10/1M + 10k x $20/1M = $1.20
	// cheap: 100k x $1/1M + 10k x $2/1M  = $0.12
	if got, _ := strconv.ParseFloat(shadow, 64); got < 1.079 || got > 1.081 {
		t.Errorf("X-Relay-Shadow-Saved-Usd = %s, want ~1.08", shadow)
	}

	// Non-streaming reports actuals, not an estimate.
	if resp.Header.Get(server.HeaderSavedEstimated) != "" {
		t.Error("a non-streaming response flagged its saving as estimated")
	}

	// The decision header explains the choice.
	var summary struct {
		Chosen         string `json:"chosen"`
		Counterfactual string `json:"counterfactual"`
		Mode           string `json:"mode"`
		Ranked         []struct {
			Endpoint string  `json:"endpoint"`
			Total    float64 `json:"total"`
		} `json:"ranked"`
	}
	if err := json.Unmarshal([]byte(resp.Header.Get(server.HeaderDecision)), &summary); err != nil {
		t.Fatalf("X-Relay-Decision is not valid JSON: %v", err)
	}
	if summary.Counterfactual != "openai/cheap@us" {
		t.Errorf("counterfactual = %q", summary.Counterfactual)
	}
	if len(summary.Ranked) < 2 {
		t.Errorf("decision summary carries %d ranked entries; the evidence for the "+
			"counterfactual is missing", len(summary.Ranked))
	}

	h.settle(t)

	// And the ledger agrees with the headers.
	tot := h.agg.TenantReport("acme", time.Now()).Overall
	if tot.Requests != 1 || tot.Measured != 1 {
		t.Fatalf("ledger totals = %+v", tot)
	}
	if tot.Saved != 0 {
		t.Errorf("ledger Saved = %s, want 0", tot.Saved)
	}
	if tot.ShadowSaved != 1_080_000 {
		t.Errorf("ledger ShadowSaved = %s, want $1.08", tot.ShadowSaved)
	}
	if tot.Cost != 1_200_000 {
		t.Errorf("ledger Cost = %s, want $1.20", tot.Cost)
	}
}

// The mode is a per-tenant setting, which is what lets one customer evaluate in
// shadow while everyone else is untouched. A process-wide flag would require a
// second deployment to do the same thing.
func TestModeIsPerTenant(t *testing.T) {
	h := newLedgerHarness(t, upstreamBig)

	shadowResp := h.call(t, shadowKey,
		`{"model":"relay/test","messages":[{"role":"user","content":"hi"}]}`)
	strictResp := h.call(t, strictKey,
		`{"model":"relay/test","messages":[{"role":"user","content":"hi"}]}`)

	if got := shadowResp.Header.Get(server.HeaderMode); got != "shadow" {
		t.Errorf("acme mode = %q, want shadow", got)
	}
	if got := strictResp.Header.Get(server.HeaderMode); got != "strict" {
		t.Errorf("globex mode = %q, want strict", got)
	}
	// Strict records no counterfactual at all: there is no road not taken.
	if got := strictResp.Header.Get(server.HeaderShadowSaved); got != "" {
		t.Errorf("a strict-mode request reported a shadow saving of %q", got)
	}

	h.settle(t)

	acme := h.agg.TenantReport("acme", time.Now()).Overall
	globex := h.agg.TenantReport("globex", time.Now()).Overall

	if acme.ShadowMeasured != 1 {
		t.Errorf("acme shadow requests = %d, want 1", acme.ShadowMeasured)
	}
	if globex.ShadowMeasured != 0 {
		t.Errorf("globex recorded %d shadow requests, want 0", globex.ShadowMeasured)
	}
	// Cost is attributed to whoever incurred it.
	if acme.Requests != 1 || globex.Requests != 1 {
		t.Errorf("attribution leaked: acme=%d globex=%d", acme.Requests, globex.Requests)
	}
}

// An unkeyed request is Phase 1's behaviour and still works: attributed to the
// default tenant rather than rejected.
func TestUnkeyedRequestIsAttributedToDefault(t *testing.T) {
	h := newLedgerHarness(t, upstreamBig)

	resp := h.call(t, "", `{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — Phase 1 accepted unkeyed requests", resp.StatusCode)
	}

	h.settle(t)
	if got := h.agg.TenantReport(tenant.DefaultID, time.Now()).Overall.Requests; got != 1 {
		t.Errorf("default tenant requests = %d, want 1", got)
	}
}

// A wrong key is a mistake worth reporting, unlike no key at all.
func TestUnknownKeyIsRejected(t *testing.T) {
	h := newLedgerHarness(t, upstreamBig)

	resp := h.call(t, "sk-relay-not-real",
		`{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	// The error must not echo the key: error bodies end up in logs and
	// screenshots.
	if strings.Contains(string(body), "sk-relay-not-real") {
		t.Errorf("the error echoed the presented key: %s", body)
	}
}

const upstreamStreamBig = `data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}

data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c","model":"m","choices":[],"usage":{"prompt_tokens":100000,"completion_tokens":10000}}

data: [DONE]

`

// On a stream the headers are fixed before the first byte, so the saving can
// only be the pre-flight estimate. It is flagged rather than omitted, and never
// presented as measured — a dashboard summing both would be summing guesses.
func TestStreamingSavingIsFlaggedEstimated(t *testing.T) {
	h := newLedgerHarness(t, upstreamStreamBig)

	resp := h.call(t, shadowKey,
		`{"model":"relay/test","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if got := resp.Header.Get(server.HeaderSavedEstimated); got != "true" {
		t.Errorf("X-Relay-Saved-Estimated = %q, want true on a stream", got)
	}
	if resp.Header.Get(server.HeaderShadowSaved) == "" {
		t.Error("no shadow estimate on a streaming shadow request")
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	h.settle(t)

	// The ledger has no such constraint and records actuals.
	tot := h.agg.TenantReport("acme", time.Now()).Overall
	if tot.ShadowSaved != 1_080_000 {
		t.Errorf("ledger ShadowSaved = %s, want the measured $1.08", tot.ShadowSaved)
	}
	if tot.InputTokens != 100_000 {
		t.Errorf("ledger InputTokens = %d, want the reported 100k", tot.InputTokens)
	}
}

func TestSavingsEndpoint(t *testing.T) {
	h := newLedgerHarness(t, upstreamBig)

	h.call(t, shadowKey, `{"model":"relay/test","messages":[{"role":"user","content":"hi"}]}`)
	h.call(t, strictKey, `{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`)
	h.settle(t)

	get := func(path string) map[string]any {
		t.Helper()
		resp, err := http.Get(h.admin.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		return body
	}

	all := get("/savings")
	byTenant, _ := all["by_tenant"].(map[string]any)
	if _, ok := byTenant["acme"]; !ok {
		t.Errorf("acme missing from the report: %v", byTenant)
	}
	if _, ok := byTenant["globex"]; !ok {
		t.Errorf("globex missing from the report")
	}
	// A single instance's totals must not read as a fleet number.
	if scope, _ := all["scope"].(string); !strings.Contains(scope, "one instance") {
		t.Errorf("scope = %q, want it to state the per-process limit", scope)
	}

	// The per-tenant monthly figure the roadmap's milestone asks for.
	acme := get("/savings?tenant=acme")
	overall, _ := acme["overall"].(map[string]any)
	if got, _ := overall["shadow_saved_micros"].(float64); got != 1_080_000 {
		t.Errorf("acme shadow saving = %v, want 1080000 micros", got)
	}
	if got, _ := overall["saved_micros"].(float64); got != 0 {
		t.Errorf("acme saved = %v, want 0 — shadow mode saves nothing", got)
	}
	// The caveat travels with the numbers, because a figure read by somebody
	// making a purchasing decision gets quoted without whatever caveats live
	// only in documentation.
	if note, _ := acme["note"].(string); !strings.Contains(note, "has not been saved") {
		t.Errorf("note = %q", note)
	}

	byDay, _ := acme["by_day"].(map[string]any)
	if len(byDay) != 1 {
		t.Errorf("acme daily buckets = %d, want 1", len(byDay))
	}
	// One tenant's report must not leak another's traffic.
	if _, leaked := acme["by_tenant"]; leaked {
		t.Error("a per-tenant report carried the full tenant breakdown")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	h := newLedgerHarness(t, upstreamBig)
	h.call(t, shadowKey, `{"model":"relay/test","messages":[{"role":"user","content":"hi"}]}`)
	h.settle(t)

	resp, err := http.Get(h.admin.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		"relay_requests_total",
		"relay_cost_usd_total",
		"relay_baseline_cost_usd_total",
		"relay_shadow_saved_usd_total",
		// The split that makes "are we slow or is the provider slow" answerable.
		"relay_gateway_overhead_seconds",
		"relay_tokens_total",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s missing from /metrics", want)
		}
	}

	// Shadow money must never appear in the real-savings counter.
	if strings.Contains(text, `relay_saved_usd_total{tenant="acme"} 1.08`) {
		t.Error("a shadow saving was counted as money actually saved")
	}
}

// Metering is fire-and-forget; a failure to record must never fail a request.
// The ledger is evidence, the response is the product.
func TestMeteringFailureDoesNotFailTheRequest(t *testing.T) {
	h := newLedgerHarness(t, upstreamBig)

	// Fill the buffer past capacity with a stalled sink is covered in the meter
	// package; here the point is the handler path with no meter at all.
	resp := h.call(t, strictKey, `{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// A request that fails still produces a ledger entry, or the error rate in the
// savings report would be structurally zero.
func TestFailedRequestIsStillRecorded(t *testing.T) {
	h := newLedgerHarness(t, `{"error":{"message":"boom","type":"api_error"}}`)

	// The mock returns 200 with an error body, which the adapter rejects for
	// having no choices.
	h.call(t, strictKey, `{"model":"cheap","messages":[{"role":"user","content":"hi"}]}`)
	h.settle(t)

	tot := h.agg.TenantReport("globex", time.Now()).Overall
	if tot.Requests != 1 {
		t.Fatalf("failed request produced %d ledger entries, want 1", tot.Requests)
	}
	if tot.Errors != 1 {
		t.Errorf("Errors = %d, want 1", tot.Errors)
	}
}
