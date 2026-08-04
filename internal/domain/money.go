package domain

import "strconv"

// Money is an amount in micro-dollars (1e-6 USD).
//
// Integer arithmetic throughout. Cost is the number this system exists to
// report accurately, and float accumulation over millions of requests drifts
// in exactly the direction nobody notices until an invoice disagrees.
type Money int64

const microsPerDollar = 1_000_000

// Rate is a price in micro-dollars per million tokens. Providers quote per-1M
// prices, so storing them in the same unit keeps config load free of a
// conversion that would otherwise be a rounding site.
type Rate int64

// tokensPerRateUnit is the token count a Rate is quoted against.
const tokensPerRateUnit = 1_000_000

// Cost returns the cost of n tokens at this rate.
//
// Overflow is not a practical concern: the product of a realistic token count
// (< 2e6) and a realistic rate (< 1e9 micros, i.e. $1000 per million tokens)
// is ~2e15, three orders of magnitude below the int64 ceiling.
func (r Rate) Cost(n int) Money {
	if n <= 0 || r <= 0 {
		return 0
	}
	return Money(int64(n) * int64(r) / tokensPerRateUnit)
}

// Dollars converts to a float for display and serialization only. Never for
// arithmetic — that is what Money is for.
func (m Money) Dollars() float64 { return float64(m) / microsPerDollar }

func (m Money) String() string {
	return "$" + strconv.FormatFloat(m.Dollars(), 'f', 6, 64)
}
