package execute

import (
	"context"

	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/validate"
)

// Charge is one provider call's billable usage.
//
// A slice of these rather than a single usage on the record, because an
// escalated request made two calls and the customer was billed for both. The
// discarded attempt's tokens are the whole cost of a failed downgrade, and a
// savings ledger that dropped them would report the escalation as free — which
// would make the savings figure a marketing number rather than a measurement.
type Charge struct {
	EndpointID string
	Usage      provider.Usage
}

// Escalation records a downgrade that produced invalid output and was retried
// on the baseline.
type Escalation struct {
	// From is the endpoint whose answer failed a validity check; To is the
	// baseline it was retried on.
	From string
	To   string

	Reason validate.Reason
	Detail string

	// Recovered is whether the baseline's answer passed. False means the
	// escalation spent a second call and still returned something invalid,
	// which is a signal about the *request* rather than about the endpoint.
	Recovered bool
}

// escalate retries a failed downgrade on the baseline.
//
// Four properties, each of which is the difference between a backstop and a
// cost problem of its own:
//
//   - Sequential. The second call happens only when the first actually failed.
//     That is the entire distinction from hedging, which pays double on every
//     request and is cut in architecture §6 for exactly that reason.
//   - Once, and only to the baseline. No chains: a second escalation would mean
//     the baseline is also producing invalid output, and retrying further spends
//     money to discover that the request is the problem.
//   - Only when something was actually downgraded. Escalating from the baseline
//     to the baseline is a second identical call, and identical calls produce
//     identically invalid answers often enough that it is not worth paying to
//     find out.
//   - Both attempts are charged. The discarded tokens are the cost of the failed
//     downgrade, and they land in the ledger as a negative saving.
func (r *run) escalate(ctx context.Context, res Result) (Result, error) {
	d := r.in.Decision
	baseline := d.Baseline.EndpointID

	if r.escalated || baseline == "" || res.Response == nil {
		return res, nil
	}
	// Nothing was downgraded, so there is nowhere better to go.
	if res.Endpoint == nil || res.Endpoint.ID == baseline {
		return res, nil
	}
	// Streaming cannot be escalated. The checks that matter need the complete
	// response, and by the time it exists the client has already received most
	// of it — past the first flushed byte there is no honest way to replace what
	// was sent (ADR-0003). Stated in ADR-0009 as a real limitation rather than
	// discovered as a surprise.
	if r.in.Streaming {
		return res, nil
	}

	check := validate.Check(r.in.Request, res.Response)
	if check.Valid {
		return res, nil
	}

	r.escalated = true
	esc := Escalation{
		From:   res.Endpoint.ID,
		To:     baseline,
		Reason: check.Reason,
		Detail: check.Detail,
	}

	// The downgraded attempt is billed whether or not the retry succeeds. This
	// is recorded before the second call so that a failure of the second call
	// still leaves the first one's cost in the ledger.
	r.discarded = append(r.discarded, Charge{
		EndpointID: res.Endpoint.ID,
		Usage:      res.Response.Usage,
	})

	ep, adapter, cred, err := r.exec.prepare(ctx, r, baseline)
	if err != nil {
		// The baseline is unreachable. The downgraded answer is invalid but it
		// is what exists, and returning it is better than failing a request that
		// has an answer — the caller can judge it, and a 502 gives them nothing
		// to judge.
		r.exec.degraded("escalation", "baseline_unavailable")
		res.Escalation = &esc
		res.Discarded = r.discarded
		return res, nil
	}

	out, err := r.call(ctx, ep, adapter, cred, 0, 0)
	if err != nil {
		r.exec.degraded("escalation", "baseline_failed")
		res.Escalation = &esc
		res.Discarded = r.discarded
		return res, nil
	}

	esc.Recovered = validate.Check(r.in.Request, out.Response).Valid
	out.Escalation = &esc
	out.Discarded = r.discarded
	out.Attempts = r.attempts
	return out, nil
}

// EscalatedFrom reports the endpoint whose output failed, for the metrics label.
func (r Result) EscalatedFrom() string {
	if r.Escalation == nil {
		return ""
	}
	return r.Escalation.From
}

// DiscardedUsage sums the tokens paid for and thrown away.
func (r Result) DiscardedUsage() int {
	n := 0
	for _, c := range r.Discarded {
		n += c.Usage.InputTokens + c.Usage.OutputTokens
	}
	return n
}
