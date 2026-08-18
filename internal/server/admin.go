package server

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/respcache"
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

	// cache and stats are the two Phase 3 components whose state is otherwise
	// invisible. Both answer the same operator question — "why is the optimizer
	// not saving me anything" — which without them requires a debugger.
	cache *respcache.Store
	stats *meter.RouteStats
}

func NewAdmin(agg *meter.Aggregator, m *meter.Meter, reg *prometheus.Registry) *Admin {
	return &Admin{agg: agg, meter: m, registry: reg}
}

// WithOptimizer attaches the response cache and the route-stats histogram.
func (a *Admin) WithOptimizer(cache *respcache.Store, stats *meter.RouteStats) *Admin {
	a.cache = cache
	a.stats = stats
	return a
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

		// CachedInputShare is the fraction of purchased input tokens the
		// providers reported as cache reads.
		//
		// This is the number that says whether breakpoint insertion works.
		// breakpoints_inserted in the totals above says only that markers were
		// placed, and a marker in the wrong position is silently useless — the
		// request succeeds, the bill is unchanged, and nothing anywhere reports
		// a problem (ADR-0008).
		CachedInputShare float64 `json:"cached_input_share"`

		ResponseCache respcache.Stats `json:"response_cache"`
		CacheHitRate  float64         `json:"response_cache_hit_rate"`

		// RouteOutputP95 is the observed output length per route, and the reason
		// it is exposed: the output-ceiling lever declines to act on a route
		// without enough history, and an absent entry here is the whole
		// explanation for a lever that appears to do nothing.
		RouteOutputP95 map[string]int `json:"route_output_p95,omitempty"`

		Scope string `json:"scope"`
	}{
		Report:           rep,
		DroppedRecords:   a.meter.Dropped(),
		Incomplete:       a.meter.Dropped() > 0,
		CachedInputShare: rep.Overall.CachedInputShare(),
		ResponseCache:    a.cache.Stats(),
		CacheHitRate:     a.cache.Stats().HitRate(),
		RouteOutputP95:   a.stats.Routes(),
		// Stated so nobody reads a single instance's figures as a fleet total.
		// The durable JSONL ledger is what makes the global number
		// reconstructable until Phase 7's Postgres sink lands.
		Scope: "this process since start; behind multiple replicas this is one instance's view",
	}

	writeJSON(w, http.StatusOK, body)
}
