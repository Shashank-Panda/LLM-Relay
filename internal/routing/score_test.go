package routing

import (
	"math"
	"reflect"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
)

func TestInvertedMinMax(t *testing.T) {
	tests := []struct {
		name string
		in   []float64
		want []float64
	}{
		{
			name: "cheapest scores 1, dearest scores 0",
			in:   []float64{232500, 46500, 2100},
			want: []float64{0, 0.8072916666666666, 1},
		},
		{
			// The obvious bug: max == min gives a zero range. Every candidate
			// is equally good on this dimension, so every candidate scores 1.
			name: "identical values all score 1",
			in:   []float64{500, 500, 500},
			want: []float64{1, 1, 1},
		},
		{
			name: "single survivor scores 1",
			in:   []float64{42},
			want: []float64{1},
		},
		{
			name: "all zero scores 1",
			in:   []float64{0, 0},
			want: []float64{1, 1},
		},
		{
			name: "empty",
			in:   nil,
			want: []float64{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := invertedMinMax(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if math.Abs(got[i]-tc.want[i]) > 1e-12 {
					t.Errorf("[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestRank_TieBreakingIsDeterministic covers the documented order: total, then
// quality on the route's primary dimension, then lower cost, then endpoint ID.
func TestRank_TieBreakingIsDeterministic(t *testing.T) {
	route := &domain.Route{
		Name:    "t",
		Weights: map[string]float64{"quality.coding": 0.5, domain.DimCost: 0.5},
	}
	ep := func(id string, coding float64) *domain.ModelEndpoint {
		return &domain.ModelEndpoint{ID: id, Quality: map[string]float64{"coding": coding}}
	}

	tests := []struct {
		name   string
		scored []domain.ScoredCandidate
		cands  []candidate
		want   []string
	}{
		{
			name: "higher total wins",
			scored: []domain.ScoredCandidate{
				{EndpointID: "b", Total: 0.4},
				{EndpointID: "a", Total: 0.9},
			},
			cands: []candidate{{ep: ep("b", 0.9)}, {ep: ep("a", 0.1)}},
			want:  []string{"a", "b"},
		},
		{
			name: "equal totals break on quality",
			scored: []domain.ScoredCandidate{
				{EndpointID: "low", Total: 0.5, Cost: 10},
				{EndpointID: "high", Total: 0.5, Cost: 10},
			},
			cands: []candidate{{ep: ep("low", 0.2)}, {ep: ep("high", 0.9)}},
			want:  []string{"high", "low"},
		},
		{
			name: "equal totals and quality break on cost",
			scored: []domain.ScoredCandidate{
				{EndpointID: "dear", Total: 0.5, Cost: 900},
				{EndpointID: "cheap", Total: 0.5, Cost: 100},
			},
			cands: []candidate{{ep: ep("dear", 0.5)}, {ep: ep("cheap", 0.5)}},
			want:  []string{"cheap", "dear"},
		},
		{
			name: "otherwise identical candidates break lexicographically",
			scored: []domain.ScoredCandidate{
				{EndpointID: "zeta", Total: 0.5, Cost: 100},
				{EndpointID: "alpha", Total: 0.5, Cost: 100},
			},
			cands: []candidate{{ep: ep("zeta", 0.5)}, {ep: ep("alpha", 0.5)}},
			want:  []string{"alpha", "zeta"},
		},
		{
			// Totals differing only in the last ulp are the same total. A
			// scorer that reorders on float noise is not deterministic.
			name: "float noise does not reorder",
			scored: []domain.ScoredCandidate{
				{EndpointID: "b", Total: 0.5 + 1e-15, Cost: 100},
				{EndpointID: "a", Total: 0.5, Cost: 100},
			},
			cands: []candidate{{ep: ep("b", 0.5)}, {ep: ep("a", 0.5)}},
			want:  []string{"a", "b"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rank(tc.scored, tc.cands, route)
			got := make([]string, len(tc.scored))
			for i, c := range tc.scored {
				got[i] = c.EndpointID
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRouteQualityDim(t *testing.T) {
	tests := []struct {
		name    string
		weights map[string]float64
		want    string
	}{
		{"no quality dimension", map[string]float64{domain.DimCost: 1}, ""},
		{"single", map[string]float64{"quality.coding": 1}, "coding"},
		{
			name:    "highest weighted wins",
			weights: map[string]float64{"quality.coding": 0.2, "quality.reasoning": 0.8},
			want:    "reasoning",
		},
		{
			// Equal weights must resolve the same way every run, or the
			// tie-breaker itself becomes nondeterministic.
			name:    "equal weights break lexicographically",
			weights: map[string]float64{"quality.reasoning": 0.5, "quality.coding": 0.5},
			want:    "coding",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &domain.Route{Name: "t", Weights: tc.weights}
			if got := r.QualityDim(); got != tc.want {
				t.Errorf("QualityDim() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScore_ComponentsSumToTotal guards the audit path: a total nobody can
// reconstruct from its parts is a total nobody can dispute.
func TestScore_ComponentsSumToTotal(t *testing.T) {
	d := mustRoute(t, docInput())
	for _, c := range d.Ranked {
		sum := 0.0
		for _, k := range domain.SortedKeys(c.Components) {
			sum += c.Components[k]
		}
		if math.Abs(sum-c.Total) > 1e-12 {
			t.Errorf("%s: components sum to %v, total is %v", c.EndpointID, sum, c.Total)
		}
	}
}
