package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/server"
)

// subCatalogYAML is a downgrade with a tenfold price gap, so the savings
// arithmetic below is legible rather than merely correct.
const subCatalogYAML = `
version: "sub-1"
endpoints:
  - id: openai/dear@us
    provider: openai
    model: dear
    deployment: us
    credential_ref: openai-primary
    base_url: BASE/dear
    capabilities: {streaming: true, tools: true, json_schema: true}
    limits: {context_window: 128000, max_output_tokens: 16384}
    pricing: {input: 10.00, output: 20.00, source: test, verified_on: 2026-08-01}
    quality: {coding: 0.90}
  - id: openai/cheap@us
    provider: openai
    model: cheap
    deployment: us
    credential_ref: openai-primary
    base_url: BASE/cheap
    capabilities: {streaming: true, tools: true, json_schema: true}
    limits: {context_window: 128000, max_output_tokens: 16384}
    pricing: {input: 1.00, output: 2.00, source: test, verified_on: 2026-08-01}
    quality: {coding: 0.70}
routes:
  - name: relay/sub
    candidates: [openai/dear@us, openai/cheap@us]
    baseline: openai/dear@us
    weights: {cost: 1.0}
`

func subSite(path string) string {
	if strings.HasPrefix(path, "/cheap") {
		return "cheap"
	}
	return "dear"
}

// usage100k reports 100k input and 10k output, so at $10/$20 per 1M the dear
// endpoint costs exactly $1.20 and the cheap one exactly $0.12.
func usageJSON(site, content string) string {
	return `{"id":"chatcmpl-` + site + `","model":"` + site + `",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":` +
		strconv.Quote(content) + `},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":100000,"completion_tokens":10000}}`
}

const subBody = `{"model":"relay/sub","messages":[{"role":"user","content":"hi"}]}`

// --- the headline claim ---

func TestShadowPredictsWhatOptimizeMaterialises(t *testing.T) {
	// Phase 5's second "done when", and the product's whole sales motion: a
	// prospect runs a month in shadow, reads a figure, switches to optimize, and
	// the figure becomes real. If these two numbers disagree, every shadow report
	// ever shown to a customer was a guess.
	respond := func(site string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageJSON(site, "an answer"))
	}

	shadow := newSubHarness(t, domain.ModeShadow, respond)
	resp := shadow.post(t, subBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shadow status = %d", resp.StatusCode)
	}
	// Shadow serves the baseline, so nothing was actually saved.
	if got := resp.Header.Get(server.HeaderSaved); got != "0.000000" {
		t.Errorf("shadow %s = %q, want 0", server.HeaderSaved, got)
	}
	predicted := resp.Header.Get(server.HeaderShadowSaved)
	if predicted == "" || predicted == "0.000000" {
		t.Fatalf("shadow %s = %q, want the counterfactual", server.HeaderShadowSaved, predicted)
	}
	drainBody(t, resp)

	optimize := newSubHarness(t, domain.ModeOptimize, respond)
	resp = optimize.post(t, subBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("optimize status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get(server.HeaderSubstituted); got != "true" {
		t.Errorf("optimize %s = %q, want true", server.HeaderSubstituted, got)
	}
	realised := resp.Header.Get(server.HeaderSaved)
	drainBody(t, resp)

	// The same tokens priced the same two ways. Not "close" — identical, because
	// both figures are the same subtraction over the same catalog.
	if realised != predicted {
		t.Errorf("shadow predicted %s and optimize realised %s: a shadow report that "+
			"does not materialise is a number nobody should have been shown",
			predicted, realised)
	}

	// $1.20 at the baseline minus $0.12 served.
	if realised != "1.080000" {
		t.Errorf("saved = %s, want 1.080000", realised)
	}
}

// --- escalation economics ---

func TestEscalationRecordsANegativeSaving(t *testing.T) {
	// The most uncomfortable number in the product, and the one that makes the
	// rest of them credible. A failed downgrade costs *more* than not optimizing
	// at all, and it lands in the ledger as a loss.
	h := newSubHarness(t, domain.ModeOptimize, func(site string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if site == "cheap" {
			// Empty completion: invalid, and the trigger for escalation.
			_, _ = io.WriteString(w, usageJSON(site, ""))
			return
		}
		_, _ = io.WriteString(w, usageJSON(site, "a real answer"))
	})

	resp := h.post(t, subBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get(server.HeaderEscalated); got == "" {
		t.Fatalf("%s is absent; the caller paid for two answers and was not told",
			server.HeaderEscalated)
	}
	// The client gets the good answer. That is what the second call bought.
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	choices, _ := body["choices"].([]any)
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	if content, _ := msg["content"].(string); content != "a real answer" {
		t.Errorf("content = %q, want the baseline's answer", content)
	}

	h.settle(t)
	rec := h.lastRecord(t)

	if !rec.Escalated {
		t.Fatal("the ledger does not record the escalation")
	}
	if rec.EscalatedFrom != "openai/cheap@us" {
		t.Errorf("escalated_from = %q", rec.EscalatedFrom)
	}
	if !rec.EscalationRecovered {
		t.Error("escalation_recovered = false when the baseline's answer was valid")
	}
	// The discarded attempt cost $0.12 and is inside Cost, so the request cost
	// $1.32 against a $1.20 baseline.
	if rec.DiscardedCost != 120_000 {
		t.Errorf("discarded_cost = %s, want $0.120000", rec.DiscardedCost)
	}
	if rec.Cost != 1_320_000 {
		t.Errorf("cost = %s, want $1.320000 — both attempts", rec.Cost)
	}
	if rec.BaselineCost != 1_200_000 {
		t.Errorf("baseline_cost = %s, want $1.200000", rec.BaselineCost)
	}
	if rec.Saved != -120_000 {
		t.Errorf("saved = %s, want -$0.120000: a savings ledger that excluded its "+
			"own failures would be marketing rather than measurement", rec.Saved)
	}
}

func TestEscalationRateIsMeasuredAgainstEligibleRequests(t *testing.T) {
	// The SLO is "under 2% per route" (ADR-0009). Measured against every request
	// it would be diluted by strict traffic that was never eligible, and a route
	// escalating half its downgrades could still report 1%.
	h := newSubHarness(t, domain.ModeOptimize, func(site string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if site == "cheap" {
			_, _ = io.WriteString(w, usageJSON(site, ""))
			return
		}
		_, _ = io.WriteString(w, usageJSON(site, "fine"))
	})

	drainBody(t, h.post(t, subBody))
	h.settle(t)

	var report struct {
		EscalationRate float64 `json:"escalation_rate"`
		Overall        struct {
			Escalations   int64 `json:"escalations"`
			Substitutable int64 `json:"substitutable_requests"`
		} `json:"overall"`
		Note string `json:"note"`
	}
	getJSON(t, h.admin.URL+"/savings", &report)

	if report.Overall.Escalations != 1 || report.Overall.Substitutable != 1 {
		t.Fatalf("escalations=%d substitutable=%d, want 1 and 1",
			report.Overall.Escalations, report.Overall.Substitutable)
	}
	if report.EscalationRate != 1 {
		t.Errorf("escalation_rate = %v, want 1 for one escalation out of one eligible request",
			report.EscalationRate)
	}
	// The report says the uncomfortable part out loud rather than leaving a
	// reader to work out why savings fell.
	if !strings.Contains(report.Note, "two answers and used one") {
		t.Errorf("note = %q, want it to explain the negative saving", report.Note)
	}
}

func TestValidDowngradeIsNotEscalated(t *testing.T) {
	// The common path: a downgrade that works costs one call and saves money.
	// If this ever escalates, optimize mode costs double on every request.
	h := newSubHarness(t, domain.ModeOptimize, func(site string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageJSON(site, "a perfectly good answer"))
	})

	resp := h.post(t, subBody)
	drainBody(t, resp)

	if got := resp.Header.Get(server.HeaderEscalated); got != "" {
		t.Errorf("%s = %q on a valid downgrade", server.HeaderEscalated, got)
	}
	if got := h.hitCount("dear"); got != 0 {
		t.Errorf("the baseline was called %d times for a valid downgrade", got)
	}

	h.settle(t)
	if rec := h.lastRecord(t); rec.Saved != 1_080_000 {
		t.Errorf("saved = %s, want $1.080000", rec.Saved)
	}
}

func TestEscalationFeedsBackIntoRouting(t *testing.T) {
	// ADR-0009's control signal. An endpoint that keeps producing invalid output
	// has its asserted quality revised down by observation, and routing stops
	// choosing it — without waiting for anyone to read a dashboard.
	h := newSubHarness(t, domain.ModeOptimize, func(site string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if site == "cheap" {
			_, _ = io.WriteString(w, usageJSON(site, ""))
			return
		}
		_, _ = io.WriteString(w, usageJSON(site, "fine"))
	})

	for range 60 {
		drainBody(t, h.post(t, subBody))
	}

	penalty := h.tracker.Snapshot().For("openai/cheap@us").QualityPenalty
	if penalty == 0 {
		t.Fatal("an endpoint escalating on every request earned no quality penalty")
	}
	// The catalog asserts 0.70; observation has contradicted it.
	if got := h.tracker.Snapshot().For("openai/cheap@us").EffectiveQuality(0.70); got >= 0.70 {
		t.Errorf("effective quality = %v, want it below the asserted 0.70", got)
	}
}

func TestPinnedStrictRequestIsNeverDowngraded(t *testing.T) {
	// The escape hatch, checked on the one path where it matters most: a tenant
	// in optimize mode who needs this single request untouched.
	h := newSubHarness(t, domain.ModeOptimize, func(site string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, usageJSON(site, "ok"))
	})

	resp := h.postWith(t, subBody, map[string]string{server.HeaderPin: "strict"})
	drainBody(t, resp)

	if got := resp.Header.Get(server.HeaderEndpoint); got != "openai/dear@us" {
		t.Errorf("%s = %q, want the model that was asked for", server.HeaderEndpoint, got)
	}
	if got := resp.Header.Get(server.HeaderSubstituted); got != "" {
		t.Errorf("%s = %q under an explicit pin", server.HeaderSubstituted, got)
	}
	if got := h.hitCount("cheap"); got != 0 {
		t.Errorf("the cheap endpoint was called %d times for a pinned request", got)
	}
}
