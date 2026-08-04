package routing

import (
	"math"
	"sort"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
)

// tieEpsilon is the width below which two totals are considered equal.
// Float arithmetic makes exact comparison meaningless at the last ulp, and a
// scorer that reorders on noise is not deterministic in any useful sense.
const tieEpsilon = 1e-9

// score ranks the survivors of filtering.
//
// Each dimension is normalized to 0..1 and combined with the route's weights:
//
//	score(e) = Σ weight[d] × normalized[d](e)
//
// Normalization for cost and latency is inverted min-max *across the surviving
// set*, which has a consequence worth stating: scores are comparable within one
// decision and not across decisions. Adding an expensive candidate to a route
// compresses everyone else's cost score. Do not chart these over time.
func score(cands []candidate, req *domain.Request, rt *domain.Route) []domain.ScoredCandidate {
	if len(cands) == 0 {
		return nil
	}

	costs := make([]float64, len(cands))
	latencies := make([]float64, len(cands))
	for i, c := range cands {
		costs[i] = float64(c.cost)
		latencies[i] = float64(c.health.LatencyEWMA)
	}
	costNorm := invertedMinMax(costs)
	latencyNorm := invertedMinMax(latencies)

	// Weight keys are iterated in sorted order. Float addition is not
	// associative, so Go's randomized map order would make identical inputs
	// produce totals differing in the last ulp.
	dims := domain.SortedKeys(rt.Weights)

	out := make([]domain.ScoredCandidate, len(cands))
	for i, c := range cands {
		components := make(map[string]float64, len(dims))
		var reasons []string
		total := 0.0

		for _, d := range dims {
			w := rt.Weights[d]
			var norm float64

			switch {
			case d == domain.DimCost:
				norm = costNorm[i]
			case d == domain.DimLatency:
				norm = latencyNorm[i]
			case d == domain.DimCacheAffinity:
				if req.PreviousEndpoint != "" && req.PreviousEndpoint == c.ep.ID {
					norm = 1
					reasons = append(reasons, "session affinity from previous turn")
				}
			case strings.HasPrefix(d, domain.DimQualityPrefix):
				norm = c.ep.QualityFor(strings.TrimPrefix(d, domain.DimQualityPrefix))
			}

			contribution := w * norm
			components[d] = contribution
			total += contribution
		}

		out[i] = domain.ScoredCandidate{
			EndpointID: c.ep.ID,
			Total:      total,
			Components: components,
			Cost:       c.cost,
			Reasons:    reasons,
		}
	}

	rank(out, cands, rt)
	return out
}

// invertedMinMax normalizes lower-is-better values so that the cheapest or
// fastest scores 1 and the dearest or slowest scores 0.
//
// When every value is identical — including the single-survivor case — the
// range is zero and every candidate scores 1. Dividing by that range is the
// obvious bug here, and "they are all equally good on this dimension" is the
// only defensible reading.
func invertedMinMax(vals []float64) []float64 {
	out := make([]float64, len(vals))
	if len(vals) == 0 {
		return out
	}
	min, max := vals[0], vals[0]
	for _, v := range vals[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	if span <= 0 {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	for i, v := range vals {
		out[i] = (max - v) / span
	}
	return out
}

// rank sorts best-first with deterministic tie-breaking: total, then quality on
// the route's primary dimension, then lower cost, then endpoint ID.
//
// Never randomly, and never by map order. A router that returns different
// answers for identical inputs cannot be tested, cannot be explained to a
// customer, and cannot have a past decision replayed.
func rank(scored []domain.ScoredCandidate, cands []candidate, rt *domain.Route) {
	quality := make(map[string]float64, len(cands))
	if dim := rt.QualityDim(); dim != "" {
		for _, c := range cands {
			quality[c.ep.ID] = c.ep.QualityFor(dim)
		}
	}

	sort.SliceStable(scored, func(i, j int) bool {
		a, b := scored[i], scored[j]
		if math.Abs(a.Total-b.Total) > tieEpsilon {
			return a.Total > b.Total
		}
		if qa, qb := quality[a.EndpointID], quality[b.EndpointID]; math.Abs(qa-qb) > tieEpsilon {
			return qa > qb
		}
		if a.Cost != b.Cost {
			return a.Cost < b.Cost
		}
		return a.EndpointID < b.EndpointID
	})
}
