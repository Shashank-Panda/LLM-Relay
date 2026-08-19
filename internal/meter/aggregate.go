package meter

import (
	"sync"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// Totals is an aggregate over some set of records.
//
// Note that Requests and Measured are separate counts, and every cost figure
// below is a sum over the Measured subset only. Dividing a saving by Requests
// would understate it by however many requests had no baseline — an error that
// grows precisely as a customer adopts virtual routes.
type Totals struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`

	// Measured counts requests that had a baseline to compare against.
	// Unmeasured is not zero, so the two are never summed.
	Measured   int64 `json:"measured_requests"`
	Unmeasured int64 `json:"unmeasured_requests"`

	// EstimatedUsage counts requests whose provider returned no usage block.
	// Their cost is a guess and is excluded from precision reporting.
	EstimatedUsage int64 `json:"estimated_usage_requests"`

	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`

	Cost         domain.Money `json:"cost_micros"`
	BaselineCost domain.Money `json:"baseline_cost_micros"`

	// Saved is money actually not spent, over the measured subset.
	Saved domain.Money `json:"saved_micros"`

	// ShadowSaved is money that *would* have been saved had optimize mode been
	// enabled. A separate field all the way to the report, because summing it
	// with Saved would tell a customer in shadow mode that they had already
	// banked a saving they have not.
	ShadowSaved    domain.Money `json:"shadow_saved_micros"`
	ShadowMeasured int64        `json:"shadow_requests"`

	Substitutions int64 `json:"substitutions"`

	// BreakpointRequests counts requests that had cache markers inserted, and
	// Breakpoints the markers themselves.
	//
	// These two exist to be read against CachedInputTokens and never on their
	// own. A breakpoint in the wrong place is silently useless — the request
	// succeeds, the bill is unchanged, and no error appears anywhere — so
	// "markers inserted" is a measure of effort and "cached input tokens" is the
	// only measure of effect. ADR-0008 is explicit about this, and it is the
	// specific check that closes Phase 3.
	BreakpointRequests int64 `json:"breakpoint_requests"`
	Breakpoints        int64 `json:"breakpoints_inserted"`

	// CacheHits counts answers served from the response cache, and
	// CacheHitTokens the tokens that were therefore never bought.
	//
	// Kept out of the token sums above deliberately. InputTokens and
	// OutputTokens answer "what did we buy"; adding tokens nobody paid for would
	// make that question unanswerable and would inflate every per-token figure
	// derived from it.
	CacheHits      int64 `json:"cache_hits"`
	CacheHitTokens int64 `json:"cache_hit_tokens_avoided"`

	// Truncated counts responses that stopped because they hit an output
	// ceiling. A ceiling that fires regularly is wrong and should be raised —
	// which is only knowable if it is counted (ADR-0008).
	Truncated int64 `json:"truncated_responses"`

	// Attempts, Retries, and Failovers are reliability figures that are also
	// cost figures, which is why they belong in a savings report rather than
	// only on a dashboard. Every attempt past the first is a second charge for
	// one answer, so a retry storm shows up here as spend with no corresponding
	// output — and that is a number somebody reviewing an invoice needs.
	Attempts  int64 `json:"attempts"`
	Retries   int64 `json:"retries"`
	Failovers int64 `json:"failovers"`
	Reroutes  int64 `json:"reroutes"`

	// StreamFailuresAfterTTFT counts streams that broke after the client had
	// received content — the failure ADR-0003 knowingly does not cover, kept
	// visible so the size of that gap is measured rather than assumed.
	StreamFailuresAfterTTFT int64 `json:"stream_failures_after_ttft"`

	// Escalations counts downgrades whose output failed a validity check and
	// were retried on the baseline, and DiscardedCost is what those abandoned
	// answers cost.
	//
	// DiscardedCost is already inside Cost — it is not an extra to be added, it
	// is the part of the spend that bought nothing. Reported separately because
	// "we spent this and threw it away" is the number that says whether the
	// downgrades are worth making, and it is invisible inside a total.
	Escalations          int64        `json:"escalations"`
	EscalationsRecovered int64        `json:"escalations_recovered"`
	DiscardedCost        domain.Money `json:"discarded_cost_micros"`

	// Substitutable counts requests where something *could* have been
	// downgraded, which is the denominator the 2% escalation SLO is measured
	// against. Against total requests the rate would be diluted by every strict
	// request that was never eligible.
	Substitutable int64 `json:"substitutable_requests"`
}

// EscalationRate is escalations over the requests that were eligible for one.
//
// ADR-0009 sets the SLO at under 2% per route. Measured against substitutable
// requests rather than all of them: a tenant running mostly strict traffic would
// otherwise show a rate near zero however badly their downgrades were doing.
func (t Totals) EscalationRate() float64 {
	if t.Substitutable <= 0 {
		return 0
	}
	return float64(t.Escalations) / float64(t.Substitutable)
}

// CachedInputShare is the fraction of input tokens the provider reported as
// cache reads. This is the number that says whether breakpoint insertion is
// working; the inserted count says only that it was attempted.
func (t Totals) CachedInputShare() float64 {
	if t.InputTokens <= 0 {
		return 0
	}
	return float64(t.CachedInputTokens) / float64(t.InputTokens)
}

func (t *Totals) add(r Record) {
	t.Requests++
	if r.Outcome == OutcomeError {
		t.Errors++
	}
	if r.UsageEstimated {
		t.EstimatedUsage++
	}

	if r.Breakpoints > 0 {
		t.BreakpointRequests++
		t.Breakpoints += int64(r.Breakpoints)
	}
	if r.FinishReason == string(provider.FinishLength) {
		t.Truncated++
	}

	t.Attempts += int64(r.Attempts)
	t.Retries += int64(r.Retries)
	t.Failovers += int64(r.Failovers)
	if r.Rerouted {
		t.Reroutes++
	}
	if r.StreamFailedAfterTTFT {
		t.StreamFailuresAfterTTFT++
	}
	if r.Substituted || r.Escalated {
		// Escalated requests count as substitutable even though Endpoint now
		// names the baseline: something *was* downgraded, and the escalation is
		// the evidence.
		t.Substitutable++
	}
	if r.Escalated {
		t.Escalations++
		t.DiscardedCost += r.DiscardedCost
		if r.EscalationRecovered {
			t.EscalationsRecovered++
		}
	}

	if r.CacheHit {
		// Tokens avoided rather than tokens bought. See the field comments: the
		// two must not be added together, so they are not added together here.
		t.CacheHits++
		t.CacheHitTokens += int64(r.InputTokens + r.OutputTokens)
	} else {
		t.InputTokens += int64(r.InputTokens)
		t.CachedInputTokens += int64(r.CachedInputTokens)
		t.OutputTokens += int64(r.OutputTokens)
	}
	t.Cost += r.Cost

	if r.Substituted {
		t.Substitutions++
	}

	if !r.SavingMeasured {
		t.Unmeasured++
		return
	}
	t.Measured++
	t.BaselineCost += r.BaselineCost
	t.Saved += r.Saved

	if r.Counterfactual != "" {
		t.ShadowMeasured++
		t.ShadowSaved += r.ShadowSaved
	}
}

// SavedUSD and the rest exist so a report can be rendered without every caller
// re-deriving the conversion — and, more importantly, without any of them doing
// the arithmetic in floats before the sum.
func (t Totals) SavedUSD() float64        { return t.Saved.Dollars() }
func (t Totals) ShadowSavedUSD() float64  { return t.ShadowSaved.Dollars() }
func (t Totals) CostUSD() float64         { return t.Cost.Dollars() }
func (t Totals) BaselineCostUSD() float64 { return t.BaselineCost.Dollars() }

// Aggregator maintains running totals in memory.
//
// The durable record is the JSONL file; this exists so that answering "what did
// we save this month" does not require scanning it. It is per-process and lost
// on restart, which is stated plainly rather than hidden: behind N replicas the
// admin endpoint reports one instance's view. Phase 7's Postgres sink is what
// makes the query global, and the file is what makes it reconstructable in the
// meantime.
type Aggregator struct {
	mu sync.RWMutex

	overall  Totals
	byTenant map[string]*Totals
	byRoute  map[string]*Totals
	byDay    map[string]*Totals

	// byTenantDay is what a monthly per-tenant figure is built from.
	byTenantDay map[tenantDay]*Totals

	since time.Time
}

type tenantDay struct {
	tenant string
	day    string
}

func NewAggregator(since time.Time) *Aggregator {
	return &Aggregator{
		byTenant:    map[string]*Totals{},
		byRoute:     map[string]*Totals{},
		byDay:       map[string]*Totals{},
		byTenantDay: map[tenantDay]*Totals{},
		since:       since,
	}
}

const dayLayout = "2006-01-02"

func (a *Aggregator) Write(r Record) {
	// UTC, always. A ledger bucketed by local time produces a 23-hour and a
	// 25-hour day twice a year, and the resulting spike in a savings report is
	// indistinguishable from a real one.
	day := r.At.UTC().Format(dayLayout)

	a.mu.Lock()
	defer a.mu.Unlock()

	a.overall.add(r)
	bucket(a.byTenant, r.Tenant).add(r)
	if r.RouteName != "" {
		bucket(a.byRoute, r.RouteName).add(r)
	}
	bucket(a.byDay, day).add(r)
	bucket(a.byTenantDay, tenantDay{r.Tenant, day}).add(r)
}

func bucket[K comparable](m map[K]*Totals, k K) *Totals {
	t, ok := m[k]
	if !ok {
		t = &Totals{}
		m[k] = t
	}
	return t
}

// Report is the savings query's answer.
type Report struct {
	Since     time.Time `json:"since"`
	Generated time.Time `json:"generated_at"`

	Overall  Totals            `json:"overall"`
	ByTenant map[string]Totals `json:"by_tenant,omitempty"`
	ByRoute  map[string]Totals `json:"by_route,omitempty"`
	ByDay    map[string]Totals `json:"by_day,omitempty"`

	// Note is a plain-language caveat rendered alongside the numbers. Savings
	// reports are read by people making purchasing decisions, and a figure
	// whose limits are stated only in documentation is a figure that will be
	// quoted without them.
	Note string `json:"note,omitempty"`
}

// Snapshot renders the current totals.
func (a *Aggregator) Snapshot(now time.Time) Report {
	a.mu.RLock()
	defer a.mu.RUnlock()

	rep := Report{
		Since:     a.since,
		Generated: now,
		Overall:   a.overall,
		ByTenant:  copyTotals(a.byTenant),
		ByRoute:   copyTotals(a.byRoute),
		ByDay:     copyTotals(a.byDay),
	}
	rep.Note = noteFor(a.overall)
	return rep
}

// TenantReport renders one tenant's totals, broken down by day.
func (a *Aggregator) TenantReport(tenant string, now time.Time) Report {
	a.mu.RLock()
	defer a.mu.RUnlock()

	rep := Report{
		Since:     a.since,
		Generated: now,
		ByDay:     map[string]Totals{},
	}
	if t, ok := a.byTenant[tenant]; ok {
		rep.Overall = *t
	}
	for k, v := range a.byTenantDay {
		if k.tenant == tenant {
			rep.ByDay[k.day] = *v
		}
	}
	rep.Note = noteFor(rep.Overall)
	return rep
}

// noteFor states the caveats that apply to these particular numbers.
func noteFor(t Totals) string {
	switch {
	case t.Requests == 0:
		return "No requests recorded."
	case t.ShadowMeasured > 0 && t.Saved == 0:
		return "shadow_saved_micros is money that would have been saved had optimization " +
			"been enabled; it has not been saved. saved_micros is what was actually saved."
	case t.Unmeasured > 0:
		return "unmeasured_requests had no baseline to price against and are excluded from " +
			"every cost figure here. Unmeasured is not the same as a saving of zero."
	case t.Breakpoints > 0 && t.CachedInputTokens == 0:
		// The failure this phrasing exists to prevent: reading
		// breakpoints_inserted as evidence of savings. Markers were placed and
		// the provider reported reading none of them, which means they are in
		// the wrong place and the lever is doing nothing but adding cache
		// writes.
		return "breakpoints_inserted is above zero while cached_input_tokens is zero: markers " +
			"were placed and the provider cached none of them. Inserted is effort; " +
			"cached_input_tokens is effect."
	case t.Escalations > 0:
		// The most counter-intuitive line in the report, so it is stated rather
		// than left to be worked out: an escalated request cost more than not
		// optimizing would have, and saved_micros is lower because of it.
		return "escalated requests paid for two answers and used one. discarded_cost_micros " +
			"is already included in cost_micros and has reduced saved_micros accordingly — " +
			"a savings figure that excluded its own failures would not be a measurement."
	case t.Retries+t.Failovers > 0:
		// Stated because the obvious reading of a cost total is "what the
		// answers cost", and retried attempts are charges with no answer
		// attached to them.
		return "retries and failovers are additional provider calls; their cost is included " +
			"in cost_micros but produced no extra output."
	case t.CacheHits > 0:
		return "cache_hit_tokens_avoided is not included in input_tokens or output_tokens; " +
			"those count tokens actually purchased."
	default:
		return ""
	}
}

func copyTotals[K comparable](m map[K]*Totals) map[K]Totals {
	out := make(map[K]Totals, len(m))
	for k, v := range m {
		out[k] = *v
	}
	return out
}
