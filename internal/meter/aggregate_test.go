package meter

import (
	"strings"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
)

func at(day int) time.Time {
	return time.Date(2026, 8, day, 12, 0, 0, 0, time.UTC)
}

func shadowRecord(tenant string, day int, baseline, counterfactual domain.Money) Record {
	return Record{
		Tenant: tenant, At: at(day), RouteName: "relay/test",
		Mode:               domain.ModeShadow,
		Endpoint:           "dear",
		BaselineID:         "dear",
		Counterfactual:     "cheap",
		Cost:               baseline,
		BaselineCost:       baseline,
		Saved:              0, // the baseline was served
		CounterfactualCost: counterfactual,
		ShadowSaved:        baseline - counterfactual,
		SavingMeasured:     true,
		Outcome:            OutcomeSuccess,
	}
}

// TestShadowSavingNeverBecomesRealSaving is the single most damaging error this
// product could make: telling a customer in shadow mode that they have already
// banked a saving that only exists hypothetically.
func TestShadowSavingNeverBecomesRealSaving(t *testing.T) {
	a := NewAggregator(at(1))
	for d := 1; d <= 3; d++ {
		a.Write(shadowRecord("acme", d, 1_200_000, 120_000))
	}

	rep := a.Snapshot(at(4))

	if rep.Overall.Saved != 0 {
		t.Errorf("Saved = %s after a shadow month, want 0 — nothing was saved",
			rep.Overall.Saved)
	}
	if rep.Overall.ShadowSaved != 3*1_080_000 {
		t.Errorf("ShadowSaved = %s, want $3.24", rep.Overall.ShadowSaved)
	}
	if rep.Overall.ShadowMeasured != 3 {
		t.Errorf("ShadowMeasured = %d, want 3", rep.Overall.ShadowMeasured)
	}
	// The report is read by people making purchasing decisions. A figure whose
	// limits are stated only in documentation gets quoted without them.
	if !strings.Contains(rep.Note, "has not been saved") {
		t.Errorf("Note = %q, want it to distinguish the two figures", rep.Note)
	}
}

func TestPerTenantAndPerDayBreakdown(t *testing.T) {
	a := NewAggregator(at(1))
	a.Write(shadowRecord("acme", 1, 1_200_000, 120_000))
	a.Write(shadowRecord("acme", 2, 1_200_000, 120_000))
	a.Write(shadowRecord("globex", 1, 600_000, 60_000))

	rep := a.Snapshot(at(3))

	if got := rep.ByTenant["acme"].Requests; got != 2 {
		t.Errorf("acme requests = %d, want 2", got)
	}
	if got := rep.ByTenant["globex"].ShadowSaved; got != 540_000 {
		t.Errorf("globex shadow saving = %s, want $0.54", got)
	}
	if got := rep.ByDay["2026-08-01"].Requests; got != 2 {
		t.Errorf("day 1 requests = %d, want 2 (both tenants)", got)
	}
	if got := rep.ByRoute["relay/test"].Requests; got != 3 {
		t.Errorf("route requests = %d, want 3", got)
	}

	// This is what a monthly per-tenant figure is built from.
	tr := a.TenantReport("acme", at(3))
	if tr.Overall.ShadowSaved != 2*1_080_000 {
		t.Errorf("acme monthly shadow saving = %s, want $2.16", tr.Overall.ShadowSaved)
	}
	if len(tr.ByDay) != 2 {
		t.Errorf("acme daily buckets = %d, want 2", len(tr.ByDay))
	}
	// One tenant's report must not leak another's numbers.
	for day, tot := range tr.ByDay {
		if tot.Requests != 1 {
			t.Errorf("acme %s = %d requests, want 1 — another tenant's traffic leaked in",
				day, tot.Requests)
		}
	}
}

// Unmeasured requests are counted but excluded from every cost comparison.
// Folding them in as zeros would dilute the average saving by however many
// requests had no baseline — an error that grows as a customer adopts virtual
// routes, which is exactly when the report matters most.
func TestUnmeasuredIsExcludedFromCostFigures(t *testing.T) {
	a := NewAggregator(at(1))

	a.Write(Record{
		Tenant: "acme", At: at(1), Cost: 500_000, BaselineCost: 1_500_000,
		Saved: 1_000_000, SavingMeasured: true, Outcome: OutcomeSuccess,
	})
	a.Write(Record{
		Tenant: "acme", At: at(1), Cost: 500_000,
		SavingMeasured: false, Outcome: OutcomeSuccess,
	})

	tot := a.Snapshot(at(2)).Overall

	if tot.Requests != 2 {
		t.Errorf("Requests = %d, want 2", tot.Requests)
	}
	if tot.Measured != 1 || tot.Unmeasured != 1 {
		t.Errorf("Measured = %d, Unmeasured = %d, want 1 each", tot.Measured, tot.Unmeasured)
	}
	// The saving is over the measured subset only.
	if tot.Saved != 1_000_000 {
		t.Errorf("Saved = %s, want $1.00", tot.Saved)
	}
	if tot.BaselineCost != 1_500_000 {
		t.Errorf("BaselineCost = %s, want only the measured request's", tot.BaselineCost)
	}
	// Spend is real regardless of whether a comparison was possible.
	if tot.Cost != 1_000_000 {
		t.Errorf("Cost = %s, want both requests' spend", tot.Cost)
	}
	if !strings.Contains(rep(a).Note, "Unmeasured is not the same") {
		t.Error("the report does not flag the unmeasured exclusion")
	}
}

func rep(a *Aggregator) Report { return a.Snapshot(at(9)) }

// Cost whose provider reported no usage block is a guess. Counting it lets a
// reader exclude it rather than discovering later that a percentage of the
// figure was invented.
func TestEstimatedUsageIsCounted(t *testing.T) {
	a := NewAggregator(at(1))
	a.Write(Record{Tenant: "t", At: at(1), UsageEstimated: true, Outcome: OutcomeSuccess})
	a.Write(Record{Tenant: "t", At: at(1), Outcome: OutcomeSuccess})

	if got := a.Snapshot(at(2)).Overall.EstimatedUsage; got != 1 {
		t.Errorf("EstimatedUsage = %d, want 1", got)
	}
}

func TestErrorsAndSubstitutionsAreCounted(t *testing.T) {
	a := NewAggregator(at(1))
	a.Write(Record{Tenant: "t", At: at(1), Outcome: OutcomeError})
	a.Write(Record{Tenant: "t", At: at(1), Outcome: OutcomeSuccess, Substituted: true})
	a.Write(Record{Tenant: "t", At: at(1), Outcome: OutcomeCancelled})

	tot := a.Snapshot(at(2)).Overall
	if tot.Errors != 1 {
		t.Errorf("Errors = %d, want 1 — a cancellation is not an error", tot.Errors)
	}
	if tot.Substitutions != 1 {
		t.Errorf("Substitutions = %d, want 1", tot.Substitutions)
	}
}

// Days are bucketed in UTC. Local time produces a 23-hour and a 25-hour day
// twice a year, and the resulting spike in a savings report is
// indistinguishable from a real one.
func TestDaysAreBucketedInUTC(t *testing.T) {
	a := NewAggregator(at(1))

	// 23:30 UTC on the 1st, expressed in a zone 5.5 hours ahead — local calendar
	// says the 2nd.
	tz := time.FixedZone("IST", int((5*time.Hour + 30*time.Minute).Seconds()))
	a.Write(Record{
		Tenant:  "t",
		At:      time.Date(2026, 8, 1, 23, 30, 0, 0, time.UTC).In(tz),
		Outcome: OutcomeSuccess,
	})

	rep := a.Snapshot(at(3))
	if _, ok := rep.ByDay["2026-08-01"]; !ok {
		t.Errorf("record landed in %v, want the 2026-08-01 UTC bucket", keys(rep.ByDay))
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSnapshotIsACopy(t *testing.T) {
	a := NewAggregator(at(1))
	a.Write(Record{Tenant: "t", At: at(1), Cost: 100, Outcome: OutcomeSuccess})

	rep := a.Snapshot(at(2))
	// Mutating a snapshot must not reach back into the live aggregate, or a
	// handler rendering a report could corrupt the ledger it read from.
	rep.ByTenant["t"] = Totals{Requests: 999}
	rep.Overall.Cost = 999

	fresh := a.Snapshot(at(2))
	if fresh.ByTenant["t"].Requests != 1 || fresh.Overall.Cost != 100 {
		t.Error("mutating a snapshot reached back into the aggregator")
	}
}

func TestEmptyReportSaysSo(t *testing.T) {
	a := NewAggregator(at(1))
	if got := a.Snapshot(at(2)).Note; got != "No requests recorded." {
		t.Errorf("Note = %q", got)
	}
}
