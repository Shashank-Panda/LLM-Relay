package meter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// drainCtx bounds Close so a stuck sink fails the test rather than hanging it.
func drainCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestRecordNeverBlocks is the property the whole design exists for. A gateway
// that stalls inference to write telemetry has inverted its own priorities: the
// request is mandatory, the record about it is not.
func TestRecordNeverBlocks(t *testing.T) {
	// A sink that never returns, so the worker is stuck on the first record and
	// the buffer fills immediately.
	stuck := make(chan struct{})
	defer close(stuck)

	m := New(4, SinkFunc(func(Record) { <-stuck }))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 1000 {
			m.Record(Record{RequestID: string(rune(i))})
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Record blocked on a stalled sink — a slow disk would add latency to inference")
	}

	if m.Dropped() == 0 {
		t.Error("nothing was dropped although the buffer must have overflowed")
	}
	// Silent loss is the failure mode that matters: an incomplete ledger that
	// nobody knows is incomplete.
	t.Logf("dropped %d of 1000 with a stalled sink", m.Dropped())
}

func TestMeterDeliversToEverySink(t *testing.T) {
	var a, b atomic.Int64
	m := New(64,
		SinkFunc(func(Record) { a.Add(1) }),
		SinkFunc(func(Record) { b.Add(1) }),
	)

	for range 10 {
		m.Record(Record{Tenant: "t"})
	}
	if err := m.Close(drainCtx(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if a.Load() != 10 || b.Load() != 10 {
		t.Errorf("sinks saw %d and %d records, want 10 each", a.Load(), b.Load())
	}
	if m.Written() != 10 {
		t.Errorf("Written() = %d, want 10", m.Written())
	}
	if m.Dropped() != 0 {
		t.Errorf("dropped %d with room to spare", m.Dropped())
	}
}

// Close is called during graceful shutdown after the HTTP server has drained,
// so records for in-flight requests are already queued and must survive.
func TestCloseDrainsQueuedRecords(t *testing.T) {
	var seen atomic.Int64
	slow := SinkFunc(func(Record) {
		time.Sleep(time.Millisecond)
		seen.Add(1)
	})

	m := New(256, slow)
	for range 50 {
		m.Record(Record{Tenant: "t"})
	}
	if err := m.Close(drainCtx(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := seen.Load(); got != 50 {
		t.Errorf("%d records survived shutdown, want 50", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	m := New(8, SinkFunc(func(Record) {}))
	if err := m.Close(drainCtx(t)); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(drainCtx(t)); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A nil Meter is usable, so a caller who has not configured metering does not
// need a nil check at every call site — the one place it would eventually be
// forgotten is the panic-recovery path.
func TestNilMeterIsSafe(t *testing.T) {
	var m *Meter
	m.Record(Record{})
	if m.Dropped() != 0 || m.Written() != 0 {
		t.Error("nil meter reported counts")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Errorf("Close on nil: %v", err)
	}
}

func TestConcurrentRecording(t *testing.T) {
	var seen atomic.Int64
	m := New(8192, SinkFunc(func(Record) { seen.Add(1) }))

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				m.Record(Record{Tenant: "t"})
			}
		}()
	}
	wg.Wait()

	if err := m.Close(drainCtx(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := seen.Load() + int64(m.Dropped()); got != 1600 {
		t.Errorf("delivered+dropped = %d, want 1600 — records vanished", got)
	}
}

func catalogFixture() *domain.Catalog {
	return &domain.Catalog{
		Version: "v1",
		Endpoints: map[string]*domain.ModelEndpoint{
			// $10/1M in, $20/1M out
			"dear": {ID: "dear", Pricing: domain.Pricing{Input: 10_000_000, Output: 20_000_000}},
			// $1/1M in, $2/1M out
			"cheap": {ID: "cheap", Pricing: domain.Pricing{Input: 1_000_000, Output: 2_000_000}},
		},
	}
}

// TestPriceShadow is the arithmetic the product's central claim rests on.
func TestPriceShadow(t *testing.T) {
	cat := catalogFixture()
	d := &domain.Decision{
		Chosen:         "dear", // shadow serves the baseline
		Baseline:       domain.Baseline{EndpointID: "dear", Mode: domain.ModeShadow},
		Counterfactual: "cheap",
		SavingMeasured: true,
	}
	// 100k in, 10k out.
	u := provider.Usage{InputTokens: 100_000, OutputTokens: 10_000}

	var r Record
	r.Price(u, cat, d)

	// dear:  100k x $10/1M = $1.00  +  10k x $20/1M = $0.20  => $1.20
	if r.Cost != 1_200_000 || r.BaselineCost != 1_200_000 {
		t.Errorf("Cost = %s, BaselineCost = %s, want $1.20 each", r.Cost, r.BaselineCost)
	}
	// The baseline was served, so nothing was actually saved. Reporting the
	// counterfactual saving here would claim money that was never saved.
	if r.Saved != 0 {
		t.Errorf("Saved = %s, want 0 — shadow mode served the baseline", r.Saved)
	}

	// cheap: 100k x $1/1M = $0.10  +  10k x $2/1M = $0.02  => $0.12
	if r.CounterfactualCost != 120_000 {
		t.Errorf("CounterfactualCost = %s, want $0.12", r.CounterfactualCost)
	}
	if r.ShadowSaved != 1_080_000 {
		t.Errorf("ShadowSaved = %s, want $1.08", r.ShadowSaved)
	}
}

func TestPriceOptimizeRecordsARealSaving(t *testing.T) {
	cat := catalogFixture()
	d := &domain.Decision{
		Chosen:         "cheap",
		Baseline:       domain.Baseline{EndpointID: "dear", Mode: domain.ModeOptimize},
		SavingMeasured: true,
	}
	u := provider.Usage{InputTokens: 100_000, OutputTokens: 10_000}

	var r Record
	r.Price(u, cat, d)

	if r.Cost != 120_000 || r.BaselineCost != 1_200_000 {
		t.Errorf("Cost = %s, BaselineCost = %s", r.Cost, r.BaselineCost)
	}
	if r.Saved != 1_080_000 {
		t.Errorf("Saved = %s, want $1.08 actually saved", r.Saved)
	}
	// No counterfactual outside shadow mode: the road taken is the only one.
	if r.ShadowSaved != 0 || r.Counterfactual != "" {
		t.Errorf("shadow fields populated in optimize mode: %+v", r)
	}
}

// Unmeasured is not zero. A route with no baseline must leave every cost
// comparison empty rather than reporting a saving of nothing, because a report
// that sums the two silently dilutes every real measurement it contains.
func TestPriceUnmeasuredLeavesComparisonsEmpty(t *testing.T) {
	cat := catalogFixture()
	d := &domain.Decision{Chosen: "cheap", SavingMeasured: false}

	var r Record
	r.Price(provider.Usage{InputTokens: 100_000}, cat, d)

	if r.SavingMeasured {
		t.Error("SavingMeasured is true without a baseline")
	}
	if r.BaselineCost != 0 || r.Saved != 0 {
		t.Errorf("BaselineCost = %s, Saved = %s, want both zero and flagged unmeasured",
			r.BaselineCost, r.Saved)
	}
	// The served cost is still real and still recorded.
	if r.Cost == 0 {
		t.Error("Cost was not recorded for an unmeasured request")
	}
}

// A baseline naming an endpoint the catalog no longer contains — a retired
// model, a reload between decision and metering — must downgrade to unmeasured
// rather than price against a zero-cost phantom, which would report the entire
// spend as a loss.
func TestPriceMissingBaselineDowngradesToUnmeasured(t *testing.T) {
	cat := catalogFixture()
	d := &domain.Decision{
		Chosen:         "cheap",
		Baseline:       domain.Baseline{EndpointID: "retired-yesterday"},
		SavingMeasured: true,
	}

	var r Record
	r.Price(provider.Usage{InputTokens: 100_000}, cat, d)

	if r.SavingMeasured {
		t.Error("SavingMeasured stayed true with a baseline missing from the catalog")
	}
	if r.Saved != 0 {
		t.Errorf("Saved = %s, want 0", r.Saved)
	}
}

// time.Duration marshals as nanoseconds, so a field named *_ms carrying one is
// wrong by a factor of a million — and plausibly wrong, which is worse.
func TestDurationFieldsAreLabelledInNanoseconds(t *testing.T) {
	buf, err := json.Marshal(Record{
		Duration:         2500 * time.Millisecond,
		ProviderDuration: 2000 * time.Millisecond,
		TTFT:             300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for field, want := range map[string]float64{
		"duration_ns":          2.5e9,
		"provider_duration_ns": 2e9,
		"ttft_ns":              3e8,
	} {
		v, ok := got[field]
		if !ok {
			t.Errorf("%s missing from %s", field, buf)
			continue
		}
		if v.(float64) != want {
			t.Errorf("%s = %v, want %v", field, v, want)
		}
	}
	// A millisecond-labelled field carrying nanoseconds is the specific
	// mistake this test exists to prevent.
	for _, wrong := range []string{"duration_ms", "provider_duration_ms", "ttft_ms"} {
		if _, present := got[wrong]; present {
			t.Errorf("%s is labelled milliseconds but carries nanoseconds", wrong)
		}
	}
}

func TestOverheadClampsAtZero(t *testing.T) {
	r := Record{Duration: 10 * time.Millisecond, ProviderDuration: 8 * time.Millisecond}
	if got := r.Overhead(); got != 2*time.Millisecond {
		t.Errorf("Overhead = %v, want 2ms", got)
	}
	// A clock adjustment can make the subtraction negative, and a negative
	// overhead in a histogram is worse than a zero one.
	weird := Record{Duration: time.Millisecond, ProviderDuration: time.Second}
	if got := weird.Overhead(); got != 0 {
		t.Errorf("Overhead = %v, want it clamped to 0", got)
	}
}

func TestFileSinkWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ledger.jsonl")

	s, err := OpenFile(path, 2)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	for i := range 5 {
		s.Write(Record{
			RequestID: "req", Tenant: "acme", At: time.Unix(int64(i), 0).UTC(),
			Cost: 1000, BaselineCost: 5000, Saved: 4000, SavingMeasured: true,
		})
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", n, err)
		}
		if r.Tenant != "acme" || r.Saved != 4000 {
			t.Errorf("line %d round-tripped as %+v", n, r)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning the ledger: %v", err)
	}
	if n != 5 {
		t.Errorf("read %d lines, want 5", n)
	}
}

// TestFileSinkIsReadableWithoutClosing is a regression.
//
// An earlier version buffered until 64 records had accumulated, so a freshly
// started gateway looked like it was recording nothing at all, `tail -f` showed
// silence, and a hard kill lost the lot. The justification was that fsync
// belongs off the hot path — true, but the sink already runs on the meter's
// worker goroutine, so the request path was never the thing being protected.
func TestFileSinkIsReadableWithoutClosing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")

	s, err := OpenFile(path, 1000) // fsync effectively never
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer s.Close()

	s.Write(Record{Tenant: "acme", At: time.Unix(0, 0), Saved: 4000, SavingMeasured: true})

	// Read it back with the sink still open and nothing flushed explicitly.
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if len(buf) == 0 {
		t.Fatal("the ledger is empty while the sink is open — records are stranded in a buffer")
	}

	var r Record
	if err := json.Unmarshal(bytes.TrimSpace(buf), &r); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if r.Tenant != "acme" {
		t.Errorf("round-tripped as %+v", r)
	}
}

// The ledger holds per-tenant cost data. Not secrets, but not world-readable.
func TestFileSinkPermissions(t *testing.T) {
	if os.Getenv("OS") == "Windows_NT" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	s, err := OpenFile(path, 0)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}
