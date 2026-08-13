package domain

// Optimization records one adjustment the optimizer made to a request.
//
// Recorded, not merely applied. An optimization the customer cannot see is
// indistinguishable from a bug — and when someone asks why a response was
// shorter than yesterday's, this is the answer.
type Optimization struct {
	Lever  string
	Before string
	After  string
	Reason string
}

// Lever names, used in Optimization.Lever and in the metrics label.
const (
	LeverCacheBreakpoints = "cache_breakpoints"
	LeverEffort           = "effort"
	LeverMaxTokens        = "max_tokens"
	LeverContextPrune     = "context_prune"
)

// LeverConfig selects which optimizations are enabled and bounds each one.
//
// The zero value enables nothing. That is the same posture as BaselineMode
// defaulting to strict: a tenant should never discover that their requests are
// being rewritten because somebody left a field blank.
type LeverConfig struct {
	CacheBreakpoints bool
	EffortDownshift  bool
	OutputCeiling    bool
	ContextPruning   bool

	// MinCacheableTokens is the smallest prefix worth marking. Providers
	// impose their own minimum before a cache write is honoured, and a write
	// that is never read is pure overhead.
	MinCacheableTokens int

	// MaxBreakpoints caps how many markers are placed. Providers allow only a
	// handful, and each one costs a cache write.
	MaxBreakpoints int

	// DefaultEffort is applied when the caller specified no reasoning effort.
	// It is a configured default, not an inference: until the classifier
	// exists, guessing that a request needs less thinking is not something
	// this component is entitled to do.
	DefaultEffort ReasoningEffort

	// OutputCeilingSlack multiplies the observed p95 output length to leave
	// headroom. Below 1 it would truncate the majority of responses.
	OutputCeilingSlack float64

	// MinOutputCeiling and MaxOutputCeiling bound the computed ceiling, so a
	// route with thin or absent history cannot produce an absurd value.
	MinOutputCeiling int
	MaxOutputCeiling int

	// KeepTurns is how many trailing messages context pruning retains.
	KeepTurns int
}

// RecommendedLevers enables the optimizations that do not change what the model
// is asked — cache breakpoints and an output ceiling — and leaves the two that
// do (effort, pruning) switched off.
func RecommendedLevers() LeverConfig {
	return LeverConfig{
		CacheBreakpoints:   true,
		OutputCeiling:      true,
		MinCacheableTokens: 1024,
		MaxBreakpoints:     4,
		OutputCeilingSlack: 1.5,
		MinOutputCeiling:   512,
		MaxOutputCeiling:   16384,
		KeepTurns:          20,
	}
}
