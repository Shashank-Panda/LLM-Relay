package meter

import (
	"sync"
)

// RouteStats tracks the distribution of output lengths per route.
//
// It exists because one optimizer lever — the `max_tokens` ceiling — is
// otherwise dead code. The lever's rule is "never below what this route's own
// responses have historically needed", and with no source of history it
// correctly declines to act on every request. So the ceiling ships with the
// thing that measures the ceiling, or it does not ship.
//
// A histogram rather than a reservoir of samples. Three reasons, in order of how
// much they matter:
//
//   - Bounded memory per route, independent of traffic. A reservoir large enough
//     to estimate a p95 well is a few thousand ints per route held forever.
//   - Reads are O(buckets) with no sorting, so the optimizer can ask on the hot
//     path inside its own budget.
//   - Quantiles round *up* to a bucket edge, and for this particular consumer
//     that is the safe direction: the number becomes an output ceiling, and
//     overestimating it wastes a little headroom while underestimating it
//     truncates somebody's answer.
type RouteStats struct {
	mu      sync.RWMutex
	byRoute map[string]*histogram

	// minSamples is how much history is required before a percentile is
	// reported at all. Below it OutputP95 returns false, because two data points
	// are not a distribution and a ceiling derived from them would cap a route
	// at whatever its first two answers happened to be.
	minSamples int
}

// DefaultMinSamples is the history a route needs before its p95 is trusted.
//
// Fifty, which is small enough that a route becomes optimizable within minutes
// of real traffic and large enough that a single outlier cannot set the ceiling.
const DefaultMinSamples = 50

func NewRouteStats(minSamples int) *RouteStats {
	if minSamples <= 0 {
		minSamples = DefaultMinSamples
	}
	return &RouteStats{byRoute: map[string]*histogram{}, minSamples: minSamples}
}

// Write implements Sink, so the distribution is fed by the same records that
// feed the ledger and the metrics. One source of truth, three consumers.
func (s *RouteStats) Write(r Record) {
	if s == nil || r.RouteName == "" {
		return
	}
	// Only completed provider calls describe what a model actually produces.
	//
	// An error produced no output at all; a cancelled stream produced a prefix
	// of one, and counting a truncated length as an observation would drag the
	// p95 down every time a client hung up — which would lower the ceiling,
	// which would truncate more responses. That feedback loop runs the wrong
	// way and it runs on its own.
	if r.Outcome != OutcomeSuccess || r.OutputTokens <= 0 {
		return
	}
	// A cache hit is a replay of an earlier answer. Counting it again would let
	// one popular prompt reshape the whole route's distribution in proportion to
	// how often it is repeated.
	if r.CacheHit {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	h, ok := s.byRoute[r.RouteName]
	if !ok {
		h = &histogram{}
		s.byRoute[r.RouteName] = h
	}
	h.observe(r.OutputTokens)
}

// OutputP95 implements optimize.StatsSource.
//
// Returns false rather than a small number when there is no basis: absent
// history and a short history are the same answer as far as the lever is
// concerned, and it is "do not act".
func (s *RouteStats) OutputP95(route string) (int, bool) {
	if s == nil {
		return 0, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	h, ok := s.byRoute[route]
	if !ok || h.total < float64(s.minSamples) {
		return 0, false
	}
	return h.quantile(0.95)
}

// Routes lists the routes with enough history to be optimizable, for the admin
// report. An operator asking why a lever is not firing wants this answer
// without attaching a debugger.
func (s *RouteStats) Routes() map[string]int {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]int, len(s.byRoute))
	for name, h := range s.byRoute {
		if h.total < float64(s.minSamples) {
			continue
		}
		if p95, ok := h.quantile(0.95); ok {
			out[name] = p95
		}
	}
	return out
}

// bucketEdges are inclusive upper bounds on output-token counts.
//
// Roughly geometric, denser where real answers live. The top edge is 65536,
// which is above every current model's output limit; a distribution whose p95
// lands in the overflow bucket has no computable ceiling and OutputP95 says so
// rather than clamping to the last edge — a route that regularly produces more
// than the largest bucket is precisely the route that must not be capped.
var bucketEdges = [...]int{
	16, 32, 64, 128, 192, 256, 384, 512, 768, 1024,
	1536, 2048, 3072, 4096, 6144, 8192, 12288, 16384,
	24576, 32768, 49152, 65536,
}

// histogram counts observations per bucket, plus an overflow.
//
// Counts are float64 so they can be halved during decay without the rounding
// that integer division would introduce — repeated integer halving drives small
// buckets to zero and erases the tail of the distribution, which is the part a
// p95 is made of.
type histogram struct {
	counts [len(bucketEdges) + 1]float64
	total  float64
}

// decayAt is the observation count at which every bucket is halved.
//
// This is what makes the statistic follow the workload instead of averaging over
// all history. A route whose prompts change shape — a new feature ships, a
// prompt gets rewritten — should re-learn its output length in thousands of
// requests, not never. Halving keeps the shape of the distribution while
// letting recent traffic outweigh old.
const decayAt = 4096

func (h *histogram) observe(tokens int) {
	h.counts[bucketFor(tokens)]++
	h.total++

	if h.total >= decayAt {
		for i := range h.counts {
			h.counts[i] /= 2
		}
		h.total /= 2
	}
}

func bucketFor(tokens int) int {
	for i, edge := range bucketEdges {
		if tokens <= edge {
			return i
		}
	}
	return len(bucketEdges) // overflow
}

// quantile returns the upper edge of the bucket containing the qth quantile.
//
// Not interpolated. Interpolating between edges would produce a more precise
// number that is not more accurate — the underlying data is bucketed and the
// precision would be invented — and the consumer wants a conservative ceiling
// rather than a best guess.
func (h *histogram) quantile(q float64) (int, bool) {
	if h.total <= 0 {
		return 0, false
	}
	target := h.total * q

	cum := 0.0
	for i, c := range h.counts {
		cum += c
		if cum < target {
			continue
		}
		if i == len(bucketEdges) {
			// The quantile is in the overflow bucket: this route produces
			// answers longer than the largest edge often enough that no ceiling
			// derived from here would be safe.
			return 0, false
		}
		return bucketEdges[i], true
	}
	return 0, false
}

// Ensure the two contracts hold at compile time rather than at the one place
// each is wired up. The StatsSource side is asserted in the gateway, which is
// where that import belongs.
var _ Sink = (*RouteStats)(nil)
