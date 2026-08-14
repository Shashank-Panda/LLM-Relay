package server

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Shashank-Panda/relay/internal/meter"
)

// Admin serves metrics and the savings query.
//
// A separate listener, bound to loopback by default. Two reasons, and the
// second is the important one:
//
//   - Cost data is not public. Phase 1 has no authentication worth the name,
//     and an unauthenticated /savings on the data plane's port would expose
//     every tenant's spend to anything that can reach it. Binding to loopback
//     is a real control; a path prefix is not.
//   - It is the control plane. Architecture §1 separates the two so that one
//     can be scaled, restarted, and firewalled independently of the other, and
//     mounting the control plane inside the data plane's mux would make that
//     separation a comment rather than a property.
type Admin struct {
	agg      *meter.Aggregator
	meter    *meter.Meter
	registry *prometheus.Registry
}

func NewAdmin(agg *meter.Aggregator, m *meter.Meter, reg *prometheus.Registry) *Admin {
	return &Admin{agg: agg, meter: m, registry: reg}
}

func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()

	if a.registry != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(a.registry, promhttp.HandlerOpts{}))
	}
	mux.HandleFunc("GET /savings", a.handleSavings)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return mux
}

// handleSavings answers "what did we save".
//
// GET /savings                → every tenant, plus per-route and per-day rollups
// GET /savings?tenant=acme    → one tenant, broken down by day
//
// This is the query the roadmap's first sellable milestone needs: enough to
// produce a per-tenant monthly figure. Its limits are stated in the response
// rather than only in documentation, because a number read by somebody making a
// purchasing decision will be quoted without whatever caveats live elsewhere.
func (a *Admin) handleSavings(w http.ResponseWriter, r *http.Request) {
	if a.agg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "metering is not enabled",
		})
		return
	}

	now := time.Now().UTC()

	var rep meter.Report
	if t := r.URL.Query().Get("tenant"); t != "" {
		rep = a.agg.TenantReport(t, now)
	} else {
		rep = a.agg.Snapshot(now)
	}

	// A dropped record is a hole in the ledger, and a report that does not
	// admit to one is a report that overstates its own completeness.
	body := struct {
		meter.Report
		DroppedRecords uint64 `json:"dropped_records"`
		Incomplete     bool   `json:"incomplete,omitempty"`
		Scope          string `json:"scope"`
	}{
		Report:         rep,
		DroppedRecords: a.meter.Dropped(),
		Incomplete:     a.meter.Dropped() > 0,
		// Stated so nobody reads a single instance's figures as a fleet total.
		// The durable JSONL ledger is what makes the global number
		// reconstructable until Phase 7's Postgres sink lands.
		Scope: "this process since start; behind multiple replicas this is one instance's view",
	}

	writeJSON(w, http.StatusOK, body)
}
