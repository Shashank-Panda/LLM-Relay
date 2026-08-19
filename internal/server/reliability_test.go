package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Shashank-Panda/relay/internal/admit"
	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/health"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/metrics"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/server"
)

// relCatalogYAML has two endpoints on one route so failover has somewhere to go.
const relCatalogYAML = `
version: "rel-1"
endpoints:
  # Two deployments of the SAME model. A strict tenant may be moved between
  # these during an outage: the model that answers is the one they named.
  - id: openai/pinned@us
    provider: openai
    model: pinned-model
    deployment: us
    credential_ref: openai-primary
    base_url: BASE/us
    capabilities: {streaming: true, tools: true, json_schema: true}
    limits: {context_window: 128000, max_output_tokens: 16384}
    pricing: {input: 2.00, output: 4.00, source: test, verified_on: 2026-08-01}
    quality: {coding: 0.80}
  - id: openai/pinned@eu
    provider: openai
    model: pinned-model
    deployment: eu
    credential_ref: openai-primary
    base_url: BASE/eu
    capabilities: {streaming: true, tools: true, json_schema: true}
    limits: {context_window: 128000, max_output_tokens: 16384}
    pricing: {input: 2.00, output: 4.00, source: test, verified_on: 2026-08-01}
    quality: {coding: 0.80}
  # A DIFFERENT model. Reachable only by a tenant who granted permission to
  # substitute, which is the line the equivalence rule draws.
  - id: openai/other@us
    provider: openai
    model: other-model
    deployment: us
    credential_ref: openai-primary
    base_url: BASE/other
    capabilities: {streaming: true, tools: true, json_schema: true}
    limits: {context_window: 128000, max_output_tokens: 16384}
    pricing: {input: 1.00, output: 2.00, source: test, verified_on: 2026-08-01}
    quality: {coding: 0.80}
routes:
  - name: relay/rel
    candidates: [openai/pinned@us, openai/pinned@eu, openai/other@us]
    baseline: openai/pinned@us
    weights: {cost: 1.0}
`

// site is which deployment an upstream call landed on, derived from the path
// prefix in its base_url. The model name cannot distinguish two deployments of
// one model, and distinguishing them is exactly what these tests are for.
func siteOf(path string) string {
	switch {
	case strings.HasPrefix(path, "/eu"):
		return "eu"
	case strings.HasPrefix(path, "/other"):
		return "other"
	default:
		return "us"
	}
}

// relHarness is Relay with reliability wired up, in front of two mock upstreams
// whose behaviour a test controls per endpoint.
type relHarness struct {
	relay *httptest.Server
	admin *httptest.Server

	meter   *meter.Meter
	agg     *meter.Aggregator
	prom    *prometheus.Registry
	tracker *health.Tracker
	store   *catalog.Store

	// hits counts calls per model name, which is how a failover is proved:
	// asserting on a header alone would pass even if Relay reported one
	// endpoint and called another.
	mu   sync.Mutex
	hits map[string]int

	// respond decides what an upstream does, by deployment site.
	respond func(site string, w http.ResponseWriter, r *http.Request)

	// records is every ledger entry written, so a test can assert on what the
	// ledger actually says rather than on a rollup of it.
	recMu   sync.Mutex
	records []meter.Record

	degraded atomic.Int64
}

type relOptions struct {
	admit   admit.Config
	policy  execute.Policy
	respond func(site string, w http.ResponseWriter, r *http.Request)
	// catalogMaxAge, when set, is how long the snapshot stays trustworthy.
	catalogMaxAge time.Duration
	// mode is the tenant's optimization mode. Strict by default, matching the
	// safe posture everywhere else.
	mode domain.BaselineMode
}

func newRelHarness(t *testing.T, opts relOptions) *relHarness {
	t.Helper()
	t.Setenv("RELAY_CRED_OPENAI_PRIMARY", "sk-test")

	h := &relHarness{hits: map[string]int{}, respond: opts.respond}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site := siteOf(r.URL.Path)

		h.mu.Lock()
		h.hits[site]++
		h.mu.Unlock()

		h.respond(site, w, r)
	}))
	t.Cleanup(upstream.Close)

	cat, err := catalog.Load(
		strings.NewReader(strings.ReplaceAll(relCatalogYAML, "BASE", upstream.URL)),
		catalog.Options{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store := catalog.NewStore(cat).WithMaxAge(opts.catalogMaxAge)

	promReg := prometheus.NewRegistry()
	mx := metrics.New(promReg)
	agg := meter.NewAggregator(time.Now().UTC())
	stats := meter.NewRouteStats(meter.DefaultMinSamples)
	capture := meter.SinkFunc(func(r meter.Record) {
		h.recMu.Lock()
		h.records = append(h.records, r)
		h.recMu.Unlock()
	})
	mtr := meter.New(1024, agg, mx, stats, capture)

	tracker := health.New(health.DefaultConfig())
	limiter := admit.New(opts.admit)

	degraded := func(component, reason string) {
		h.degraded.Add(1)
		mx.ObserveDegraded(component, reason)
	}

	client := upstream.Client()
	registry := provider.NewRegistry(openai.New(client))

	// The real default budget: four calls, which is three tries at one endpoint
	// plus one at the next. Capping it lower here would have made every failover
	// test fail as a budget exhaustion that looked like a routing bug.
	pol := opts.policy
	if pol.BaseBackoff == 0 {
		pol.BaseBackoff = time.Millisecond
		pol.MaxBackoff = time.Millisecond
	}

	gw := &gateway.Gateway{
		Store: store,
		Executor: execute.New(registry, &provider.EnvResolver{}).
			WithTracker(tracker).
			WithDegradedHook(degraded).
			WithAttemptHook(func(a execute.Attempt) {
				mx.ObserveAttempt(a.EndpointID, a.Retry, string(a.Class))
			}).
			WithPolicy(pol),
		Policy:     policyFor(opts.mode),
		Stats:      stats,
		Health:     tracker,
		OnDegraded: degraded,
	}

	srv := server.New(gw, store, registry, server.Options{
		Logger:  slog.New(slog.DiscardHandler),
		Meter:   mtr,
		Metrics: mx,
		Admit:   limiter,
	})

	h.relay = httptest.NewServer(srv.Handler())
	h.admin = httptest.NewServer(server.NewAdmin(agg, mtr, promReg).
		WithReliability(tracker, limiter, store).Handler())
	h.meter, h.agg, h.prom, h.tracker, h.store = mtr, agg, promReg, tracker, store

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

func policyFor(mode domain.BaselineMode) *domain.Policy {
	p := domain.DefaultPolicy()
	if mode != "" {
		p.OptimizationMode = mode
	}
	return p
}

func (h *relHarness) post(t *testing.T, body string, headers map[string]string) *http.Response {
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

func (h *relHarness) hitCount(site string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[site]
}

func (h *relHarness) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.meter.Written()+h.meter.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
}

const relBody = `{"model":"relay/rel","messages":[{"role":"user","content":"hi"}]}`
const relStreamBody = `{"model":"relay/rel","messages":[{"role":"user","content":"hi"}],"stream":true}`

func okJSON(w http.ResponseWriter, site string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl-`+site+`","model":"`+site+`",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
}

func failWith(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// --- failover ---

func TestFailoverToTheNextCandidate(t *testing.T) {
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			if site == "us" {
				failWith(w, http.StatusBadGateway, `{"error":{"message":"upstream down"}}`)
				return
			}
			okJSON(w, site)
		},
	})

	resp := h.post(t, relBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a dead endpoint must not be an outage", resp.StatusCode)
	}
	if got := resp.Header.Get(server.HeaderEndpoint); got != "openai/pinned@eu" {
		t.Errorf("%s = %q, want the endpoint that actually answered",
			server.HeaderEndpoint, got)
	}
	// A recovery, not a downgrade. The same model answered from a different
	// deployment, and reporting that as a substitution would tell the caller
	// they were served something cheaper than they asked for.
	if got := resp.Header.Get(server.HeaderSubstituted); got != "" {
		t.Errorf("%s = %q for a same-model failover", server.HeaderSubstituted, got)
	}
	if got := resp.Header.Get(server.HeaderFailover); got != "true" {
		t.Errorf("%s = %q, want true", server.HeaderFailover, got)
	}
	if h.hitCount("eu") == 0 {
		t.Error("the eu deployment was never called")
	}

	h.settle(t)
	// The ledger records what answered, not what was selected. Billing a
	// customer against a model that never ran is the failure mode.
	report := h.agg.Snapshot(time.Now())
	if report.Overall.Requests != 1 {
		t.Fatalf("requests = %d", report.Overall.Requests)
	}
}

func TestRetryThenSucceedOnTheSameEndpoint(t *testing.T) {
	var calls atomic.Int64
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			if site == "us" && calls.Add(1) == 1 {
				failWith(w, http.StatusServiceUnavailable, `{"error":{"message":"try again"}}`)
				return
			}
			okJSON(w, site)
		},
	})

	resp := h.post(t, relBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := h.hitCount("us"); got != 2 {
		t.Errorf("the us deployment was called %d times, want 2 — one failure, one recovery", got)
	}

	h.settle(t)
	// Every attempt past the first is a second charge for one answer, so a
	// rising retry rate shows up on an invoice before it shows up on an error
	// dashboard. The ledger is where those two facts can be read together.
	rec := lastLedgerRecord(t, h)
	if rec.Retries != 1 {
		t.Errorf("retries = %d, want 1", rec.Retries)
	}
	if rec.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", rec.Attempts)
	}
}

func TestTerminalErrorIsNotRetried(t *testing.T) {
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			failWith(w, http.StatusBadRequest,
				`{"error":{"message":"invalid tool schema","type":"invalid_request_error"}}`)
		},
	})

	resp := h.post(t, relBody, nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a malformed request succeeded")
	}
	// The most expensive mistake available: retrying a malformed request across
	// three providers produces three bills and one guaranteed failure.
	if got := h.hitCount("us"); got != 1 {
		t.Errorf("the us deployment was called %d times for a terminal error, want 1", got)
	}
	if got := h.hitCount("eu"); got != 0 {
		t.Errorf("the eu deployment was called %d times for a terminal error, want 0", got)
	}
}

// --- the ADR-0003 boundary ---

func TestNoFailoverAfterTheFirstStreamedByte(t *testing.T) {
	// The property ADR-0003 exists to guarantee. Once a chunk is flushed the
	// client has parsed and probably rendered it; restarting elsewhere
	// duplicates text and splicing produces output no single model generated.
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, "data: "+`{"id":"c","object":"chat.completion.chunk",`+
				`"choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			// A genuine mid-stream break. Returning normally would close the
			// body cleanly, which the reader cannot distinguish from a stream
			// that simply ended — ErrAbortHandler resets the connection, which
			// is what a provider dying mid-generation actually looks like.
			panic(http.ErrAbortHandler)
		},
	})

	resp := h.post(t, relStreamBody, nil)
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "partial") {
		t.Fatalf("the first chunk never arrived: %q", text)
	}
	if got := h.hitCount("eu"); got != 0 {
		t.Errorf("the eu deployment was called %d times after content had already reached the client", got)
	}
	// The failure is delivered inside the stream, because the status code was
	// fixed the moment the first frame went out. Closing the connection instead
	// is indistinguishable at the client from a network fault.
	if !strings.Contains(text, "[DONE]") {
		t.Error("the broken stream had no terminator; every OpenAI SDK waits for one")
	}

	h.settle(t)
	rec := lastLedgerRecord(t, h)
	// Counted on its own, because this is the residual risk ADR-0003 knowingly
	// accepts — and the ADR says the choice gets revisited with data if the gap
	// turns out larger than expected.
	if !rec.StreamFailedAfterTTFT {
		t.Error("a post-first-byte stream failure was not recorded as one")
	}
}

func TestStreamFailoverBeforeTheFirstByte(t *testing.T) {
	// The permitted half: the provider refused before any chunk existed, so the
	// client is uncommitted and the switch is invisible to it.
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			if site == "us" {
				failWith(w, http.StatusBadGateway, `{"error":{"message":"down"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+`{"id":"c","object":"chat.completion.chunk",`+
				`"choices":[{"index":0,"delta":{"content":"from eu"},"finish_reason":"stop"}]}`+
				"\n\ndata: [DONE]\n\n")
		},
	})

	resp := h.post(t, relStreamBody, nil)
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "from eu") {
		t.Errorf("stream did not come from the eu deployment: %q", body)
	}
	if got := resp.Header.Get(server.HeaderEndpoint); got != "openai/pinned@eu" {
		t.Errorf("%s = %q, want the endpoint that served", server.HeaderEndpoint, got)
	}
}

// --- circuit breaker ---

func TestBreakerRemovesAFailingEndpointFromRouting(t *testing.T) {
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			if site == "us" {
				failWith(w, http.StatusBadGateway, `{"error":{"message":"down"}}`)
				return
			}
			okJSON(w, site)
		},
	})

	// Enough traffic to clear the default breaker's minimum sample size.
	for range 40 {
		resp := h.post(t, relBody, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: a failing endpoint became an outage", resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	if !h.tracker.Snapshot().For("openai/pinned@us").CircuitOpen {
		t.Fatal("the breaker never opened on a permanently failing endpoint")
	}

	before := h.hitCount("us")
	for range 5 {
		resp := h.post(t, relBody, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	// Open endpoints are filtered out during routing rather than failed during
	// execution, so the ranked list stays honest and no attempt is wasted.
	if got := h.hitCount("us") - before; got != 0 {
		t.Errorf("the us deployment was called %d more times after its breaker opened", got)
	}

	// And an operator can see why, without attaching a debugger.
	var out struct {
		Breakers []health.Report `json:"breakers"`
	}
	getJSON(t, h.admin.URL+"/health", &out)
	found := false
	for _, b := range out.Breakers {
		if b.Endpoint == "openai/pinned@us" && b.State == health.StateOpen {
			found = true
		}
	}
	if !found {
		t.Error("/health does not report the open breaker")
	}
}

// --- load shedding ---

func TestSheddingReturns503WithRetryAfter(t *testing.T) {
	release := make(chan struct{})
	h := newRelHarness(t, relOptions{
		admit: admit.Config{
			MaxInFlight: 1,
			QueueWait:   10 * time.Millisecond,
			MaxQueued:   4,
			RetryAfter:  3 * time.Second,
		},
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			<-release
			okJSON(w, site)
		},
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp := h.post(t, relBody, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	// Let the first request occupy the only slot.
	time.Sleep(50 * time.Millisecond)

	resp := h.post(t, relBody, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	// 503 and not 429. The distinction is not pedantry: 429 says "you sent too
	// much", which blames a caller who may have sent one request, and SDK retry
	// logic treats the two differently.
	if got := resp.Header.Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want 3", got)
	}

	close(release)
	wg.Wait()

	// A shed request is not in the ledger. No provider was called and nothing
	// was spent, so counting it would dilute every per-request figure with work
	// that never happened.
	h.settle(t)
	report := h.agg.Snapshot(time.Now())
	if report.Overall.Requests != 1 {
		t.Errorf("ledger recorded %d requests, want only the one that ran",
			report.Overall.Requests)
	}
}

func TestSheddingIsCountedByGate(t *testing.T) {
	release := make(chan struct{})
	h := newRelHarness(t, relOptions{
		admit: admit.Config{MaxInFlight: 1, QueueWait: time.Millisecond, MaxQueued: 4},
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			<-release
			okJSON(w, site)
		},
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp := h.post(t, relBody, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	time.Sleep(50 * time.Millisecond)
	_ = h.post(t, relBody, nil)

	close(release)
	wg.Wait()

	if !metricPresent(t, h.prom, "relay_shed_total") {
		t.Error("relay_shed_total was never published")
	}
}

// --- fail open ---

func TestStaleCatalogDegradesRatherThanFails(t *testing.T) {
	// ADR-0010's sharpest edge. A stale snapshot does not error — it routes,
	// confidently, on prices that stopped being true, and every savings figure
	// computed against them is wrong in a way that looks entirely plausible. So
	// past its maximum age Relay stops making cost decisions and serves what
	// was asked for, which still answers every request.
	h := newRelHarness(t, relOptions{
		catalogMaxAge: time.Nanosecond,
		// Shadow mode is where staleness costs something: the counterfactual is
		// priced against the catalog, so a stale snapshot would produce a
		// savings figure computed from numbers nobody has confirmed. A
		// strict-mode tenant was never using those prices to decide anything.
		mode: domain.ModeShadow,
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			okJSON(w, site)
		},
	})
	time.Sleep(2 * time.Millisecond)

	if !h.store.Stale() {
		t.Fatal("the snapshot did not go stale")
	}

	resp := h.post(t, relBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d — a stale catalog must degrade, not fail", resp.StatusCode)
	}
	if h.degraded.Load() == 0 {
		t.Error("the degradation was not counted; a fail-open path nobody counts " +
			"is one nobody knows is being taken")
	}

	var out struct {
		Catalog struct {
			Stale bool   `json:"stale"`
			Note  string `json:"note"`
		} `json:"catalog"`
	}
	getJSON(t, h.admin.URL+"/health", &out)
	if !out.Catalog.Stale || out.Catalog.Note == "" {
		t.Errorf("/health does not surface the stale snapshot: %+v", out.Catalog)
	}
}

func TestProviderFailureIsTheOnlyErrorThatSurfaces(t *testing.T) {
	// The general rule ADR-0010 states: the only failures that may reach the
	// caller are failures of the provider call itself. Metering is off here,
	// which in an earlier design would have been a 500.
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			okJSON(w, site)
		},
	})
	// Close the meter mid-flight: the ledger is now unwritable.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = h.meter.Close(ctx)

	resp := h.post(t, relBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d — a broken savings ledger must cost the record, not the request",
			resp.StatusCode)
	}
}

// --- helpers ---

func lastLedgerRecord(t *testing.T, h *relHarness) meter.Record {
	t.Helper()
	h.recMu.Lock()
	defer h.recMu.Unlock()

	if len(h.records) == 0 {
		t.Fatal("nothing was recorded")
	}
	return h.records[len(h.records)-1]
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
}

func metricPresent(t *testing.T, reg *prometheus.Registry, name string) bool {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return true
		}
	}
	return false
}

func TestStrictNeverFailsOverToADifferentModel(t *testing.T) {
	// The end-to-end form of the routing rule. Both deployments of the pinned
	// model are dead and a cheaper model is up and listed as a route candidate —
	// a strict tenant gets an error rather than the substitution they refused.
	// Failing the request is the correct outcome here; quietly answering from
	// another model would be the bug.
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			if site == "other" {
				okJSON(w, site)
				return
			}
			failWith(w, http.StatusBadGateway, `{"error":{"message":"down"}}`)
		},
	})

	resp := h.post(t, relBody, nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a strict request was answered by a model the caller did not name")
	}
	if got := h.hitCount("other"); got != 0 {
		t.Errorf("the different model was called %d times for a strict request", got)
	}
	// Both deployments of the model they *did* name were tried, which is the
	// recovery they are entitled to.
	if h.hitCount("us") == 0 || h.hitCount("eu") == 0 {
		t.Errorf("did not try both deployments: us=%d eu=%d", h.hitCount("us"), h.hitCount("eu"))
	}
}

func TestOptimizeModeFailsOverAcrossModels(t *testing.T) {
	// The same fault with permission granted. In optimize mode the router picks
	// the cheaper model first; when that one is down, crossing back to a
	// different model is allowed — which is exactly what a strict tenant is
	// protected from in the test above.
	h := newRelHarness(t, relOptions{
		mode: domain.ModeOptimize,
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			if site == "other" {
				failWith(w, http.StatusBadGateway, `{"error":{"message":"down"}}`)
				return
			}
			okJSON(w, site)
		},
	})

	resp := h.post(t, relBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if h.hitCount("other") == 0 {
		t.Fatal("optimize mode did not choose the cheaper model to begin with")
	}
	served := resp.Header.Get(server.HeaderEndpoint)
	if !strings.HasPrefix(served, "openai/pinned@") {
		t.Errorf("%s = %q, want a deployment of the other model", server.HeaderEndpoint, served)
	}
	// Disclosed as a recovery, because a retried request is slower and dearer
	// than a clean one and a caller debugging their own latency should not have
	// to guess whether the extra time was the model or Relay.
	if got := resp.Header.Get(server.HeaderAttempts); got == "" {
		t.Error("a recovered request did not disclose that it took more than one call")
	}
}

func TestRetriesAreBlamedOnTheEndpointThatFailed(t *testing.T) {
	// The failure mode this exists to prevent: attributing a retry to whichever
	// endpoint eventually answered, which points an operator at the healthy
	// provider during an incident caused by the broken one.
	h := newRelHarness(t, relOptions{
		respond: func(site string, w http.ResponseWriter, r *http.Request) {
			if site == "us" {
				failWith(w, http.StatusServiceUnavailable, `{"error":{"message":"down"}}`)
				return
			}
			okJSON(w, site)
		},
	})

	resp := h.post(t, relBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	labels := counterLabels(t, h.prom, "relay_retries_total")
	if len(labels) == 0 {
		t.Fatal("no retries were recorded")
	}
	for _, l := range labels {
		if l["endpoint"] == "openai/pinned@eu" {
			t.Errorf("a retry was blamed on the endpoint that succeeded: %v", l)
		}
		if l["endpoint"] != "openai/pinned@us" {
			t.Errorf("unexpected retry label: %v", l)
		}
	}
}

// counterLabels returns the label sets a counter has observations for.
func counterLabels(t *testing.T, reg *prometheus.Registry, name string) []map[string]string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []map[string]string
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out = append(out, labels)
		}
	}
	return out
}
