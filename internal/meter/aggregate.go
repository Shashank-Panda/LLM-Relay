package meter

import (
	"sync"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
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
}

func (t *Totals) add(r Record) {
	t.Requests++
	if r.Outcome == OutcomeError {
		t.Errors++
	}
	if r.UsageEstimated {
		t.EstimatedUsage++
	}

	t.InputTokens += int64(r.InputTokens)
	t.CachedInputTokens += int64(r.CachedInputTokens)
	t.OutputTokens += int64(r.OutputTokens)
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
