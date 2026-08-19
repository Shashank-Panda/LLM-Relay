package server_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

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

// subHarness is Relay in substitution mode: one baseline, one cheaper
// alternative, and a mock that answers per endpoint.
type subHarness struct {
	relay   *httptest.Server
	admin   *httptest.Server
	meter   *meter.Meter
	tracker *health.Tracker

	mu      sync.Mutex
	hits    map[string]int
	records []meter.Record
}

func newSubHarness(
	t *testing.T,
	mode domain.BaselineMode,
	respond func(site string, w http.ResponseWriter, r *http.Request),
) *subHarness {
	t.Helper()
	t.Setenv("RELAY_CRED_OPENAI_PRIMARY", "sk-test")

	h := &subHarness{hits: map[string]int{}}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site := subSite(r.URL.Path)
		h.mu.Lock()
		h.hits[site]++
		h.mu.Unlock()
		respond(site, w, r)
	}))
	t.Cleanup(upstream.Close)

	cat, err := catalog.Load(
		strings.NewReader(strings.ReplaceAll(subCatalogYAML, "BASE", upstream.URL)),
		catalog.Options{})
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	store := catalog.NewStore(cat)

	promReg := prometheus.NewRegistry()
	mx := metrics.New(promReg)
	agg := meter.NewAggregator(time.Now().UTC())
	capture := meter.SinkFunc(func(r meter.Record) {
		h.mu.Lock()
		h.records = append(h.records, r)
		h.mu.Unlock()
	})
	mtr := meter.New(1024, agg, mx, capture)

	tracker := health.New(health.Config{
		// Small samples, so a test can establish a quality signal in tens of
		// requests rather than thousands.
		EscalationMinSamples: 20,
		MaxQualityPenalty:    0.5,
	})

	client := upstream.Client()
	registry := provider.NewRegistry(openai.New(client))

	pol := domain.DefaultPolicy()
	pol.OptimizationMode = mode

	gw := &gateway.Gateway{
		Store: store,
		Executor: execute.New(registry, &provider.EnvResolver{}).
			WithTracker(tracker).
			WithPolicy(execute.Policy{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		Policy: pol,
		Health: tracker,
	}

	srv := server.New(gw, store, registry, server.Options{
		Logger:  slog.New(slog.DiscardHandler),
		Meter:   mtr,
		Metrics: mx,
	})

	h.relay = httptest.NewServer(srv.Handler())
	h.admin = httptest.NewServer(server.NewAdmin(agg, mtr, promReg).Handler())
	h.meter, h.tracker = mtr, tracker

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

func (h *subHarness) post(t *testing.T, body string) *http.Response {
	return h.postWith(t, body, nil)
}

func (h *subHarness) postWith(t *testing.T, body string, headers map[string]string) *http.Response {
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

func (h *subHarness) hitCount(site string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[site]
}

func (h *subHarness) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for h.meter.Written()+h.meter.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
}

func (h *subHarness) lastRecord(t *testing.T) meter.Record {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) == 0 {
		t.Fatal("nothing was recorded")
	}
	return h.records[len(h.records)-1]
}
