package optimize

import (
	"testing"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// stepClock advances a fixed amount on every read.
//
// A fake clock rather than a real one, because the alternative is a test that
// proves a 3 ms budget by taking 3 ms — and a test whose cost is the thing it
// measures is a test somebody eventually deletes from CI.
type stepClock struct {
	step  time.Duration
	calls int
}

func (c *stepClock) now() time.Time {
	t := time.Unix(0, 0).Add(time.Duration(c.calls) * c.step)
	c.calls++
	return t
}

// panicStats is a StatsSource with a defect in it.
//
// Not a contrived vector: StatsSource is an interface the gateway supplies, and
// the whole point of ADR-0010's fail-open is that a defect in an optional
// component must not fail a request that would otherwise have succeeded.
type panicStats struct{}

func (panicStats) OutputP95(string) (int, bool) { panic("stats exploded") }

func TestBudgetStopsRemainingLevers(t *testing.T) {
	// One millisecond per clock read against a three millisecond budget. The
	// reads are: start, then one before each lever. So the first two levers run
	// and the third check trips.
	clock := &stepClock{step: time.Millisecond}
	o := New(domain.RecommendedLevers()).WithBudget(3 * time.Millisecond).WithClock(clock.now)

	res := o.Run(baseRequest(), statsWith(400))

	if res.Outcome != OutcomeOverBudget {
		t.Fatalf("Outcome = %q, want %q", res.Outcome, OutcomeOverBudget)
	}
	if !res.Degraded() {
		t.Error("over-budget pass does not report Degraded")
	}

	// The levers that already ran are kept. Each is independently valid, and
	// discarding correct work to reach a tidier state would cost the customer
	// money for nothing.
	if _, ok := find(res.Applied, domain.LeverCacheBreakpoints); !ok {
		t.Error("breakpoints were placed before the budget ran out but were discarded")
	}
	if _, ok := find(res.Applied, domain.LeverMaxTokens); ok {
		t.Error("output ceiling ran after the budget was exhausted")
	}
}

func TestBudgetIsNotConsultedWhenDisabled(t *testing.T) {
	clock := &stepClock{step: time.Hour}
	o := New(domain.RecommendedLevers()).WithBudget(0).WithClock(clock.now)

	res := o.Run(baseRequest(), statsWith(400))

	// With no budget there is nothing a clock reading could change, and two
	// unnecessary time reads on the hot path are two too many.
	if clock.calls != 0 {
		t.Errorf("clock read %d times with the budget disabled, want 0", clock.calls)
	}
	if res.Outcome != OutcomeApplied {
		t.Errorf("Outcome = %q, want %q", res.Outcome, OutcomeApplied)
	}
	if _, ok := find(res.Applied, domain.LeverMaxTokens); !ok {
		t.Error("an unbudgeted pass did not run every lever")
	}
}

func TestGenerousBudgetAppliesEverything(t *testing.T) {
	res := New(domain.RecommendedLevers()).Run(baseRequest(), statsWith(400))

	if res.Outcome != OutcomeApplied {
		t.Fatalf("Outcome = %q, want %q — the real 3ms budget is not enough for one small request",
			res.Outcome, OutcomeApplied)
	}
	// Not asserted to be above zero. On a small request the whole pass can land
	// inside one tick of the platform's clock, and a test demanding a non-zero
	// reading would be demanding that the optimizer be slow.
	if res.Elapsed < 0 || res.Elapsed > DefaultBudget {
		t.Errorf("Elapsed = %v, want within [0, %v]", res.Elapsed, DefaultBudget)
	}
}

func TestPanicServesTheOriginalRequest(t *testing.T) {
	in := baseRequest()
	before := in.InputTokens()

	res := New(domain.RecommendedLevers()).Run(in, panicStats{})

	if res.Outcome != OutcomePanicked {
		t.Fatalf("Outcome = %q, want %q", res.Outcome, OutcomePanicked)
	}
	if res.Request != in {
		t.Error("a crashed pass must serve the original request, not a partially rewritten one")
	}
	if len(res.Applied) != 0 {
		t.Errorf("Applied = %v, want nothing — a half-applied pass is not one anybody reasoned about",
			res.Applied)
	}
	// The input is untouched, so the request is still exactly what the caller
	// sent. Optimization is optional; delivering a response is not.
	if got := in.InputTokens(); got != before {
		t.Errorf("input tokens = %d, want %d: the original was mutated", got, before)
	}
	if breakpointCount(in) != 0 {
		t.Error("the crashed pass left breakpoints on the caller's request")
	}
}

func TestApplyDelegatesToRun(t *testing.T) {
	o := New(domain.RecommendedLevers())

	out, ops := o.Apply(baseRequest(), statsWith(400))
	res := o.Run(baseRequest(), statsWith(400))

	if len(ops) != len(res.Applied) {
		t.Errorf("Apply applied %d levers, Run applied %d", len(ops), len(res.Applied))
	}
	if out == nil {
		t.Fatal("Apply returned no request")
	}
}
