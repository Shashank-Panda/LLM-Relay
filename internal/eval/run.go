package eval

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// CaseResult is one case's outcome against one endpoint.
type CaseResult struct {
	CaseID    string
	Dimension string
	Weight    float64

	Runs   int
	Passed int

	// Errors counts runs where the provider call itself failed. Kept apart from
	// failures on purpose: a provider outage during an eval is not evidence
	// about the model, and folding it into the score would revise a model's
	// quality downward because a network was flaky that afternoon.
	Errors int

	Failures []string
	Cost     domain.Money
	Duration time.Duration
}

// PassRate is the share of completed runs that passed. Errored runs are excluded
// from the denominator, for the reason above.
func (r CaseResult) PassRate() float64 {
	completed := r.Runs - r.Errors
	if completed <= 0 {
		return 0
	}
	return float64(r.Passed) / float64(completed)
}

// EndpointResult is a whole suite's outcome against one endpoint.
type EndpointResult struct {
	Endpoint string
	Model    string
	Cases    []CaseResult
	Cost     domain.Money
	Duration time.Duration
}

// Scores reduces the case results to one score per catalog quality dimension.
//
// A weighted pass rate, which is deliberately the least clever thing that could
// work. The number ends up in a catalog as `quality.coding: 0.82` and is quoted
// to a customer as part of a claim that their output will not degrade — so it
// has to be a number they can recompute from the published case list, not the
// output of a scoring model somebody tuned.
func (r EndpointResult) Scores() map[string]float64 {
	type acc struct{ passed, total float64 }
	byDim := map[string]*acc{}

	for _, c := range r.Cases {
		completed := c.Runs - c.Errors
		if completed <= 0 {
			// Every run of this case errored. It contributes nothing rather
			// than counting as a failure — see CaseResult.Errors.
			continue
		}
		a, ok := byDim[c.Dimension]
		if !ok {
			a = &acc{}
			byDim[c.Dimension] = a
		}
		a.passed += c.Weight * float64(c.Passed)
		a.total += c.Weight * float64(completed)
	}

	out := make(map[string]float64, len(byDim))
	for dim, a := range byDim {
		if a.total > 0 {
			out[dim] = a.passed / a.total
		}
	}
	return out
}

// Errored reports how many runs failed to complete, across every case.
func (r EndpointResult) Errored() int {
	n := 0
	for _, c := range r.Cases {
		n += c.Errors
	}
	return n
}

// Runner executes a suite against endpoints.
type Runner struct {
	Registry *provider.Registry
	Resolver provider.Resolver

	// Timeout bounds one case run.
	Timeout time.Duration

	// Progress receives a line per completed case. Nil is quiet.
	Progress func(string)
}

// Run executes every case against one endpoint.
//
// Sequential, and that is not an oversight. An eval is a small number of calls
// against one provider at a time, and running them concurrently risks tripping
// the provider's own rate limit — which produces errors that look like quality
// failures unless somebody notices the pattern. The harness is slow and correct
// rather than fast and occasionally wrong about what it is measuring.
func (r *Runner) Run(ctx context.Context, s *Suite, ep *domain.ModelEndpoint) (EndpointResult, error) {
	adapter, err := r.Registry.For(ep.Provider)
	if err != nil {
		return EndpointResult{}, fmt.Errorf("eval %s: %w", ep.ID, err)
	}
	// The eval harness is a single-tenant offline tool: it resolves against
	// whatever the operator running it has configured, which is what the empty
	// tenant means here.
	cred, err := r.Resolver.Resolve(ctx, "", ep)
	if err != nil {
		return EndpointResult{}, fmt.Errorf("eval %s: %w", ep.ID, err)
	}

	out := EndpointResult{Endpoint: ep.ID, Model: ep.Model}
	started := time.Now()

	for _, c := range s.Cases {
		res := r.runCase(ctx, s, c, ep, adapter, cred)
		out.Cases = append(out.Cases, res)
		out.Cost += res.Cost

		if r.Progress != nil {
			r.Progress(fmt.Sprintf("%-28s %-14s %d/%d  %s",
				c.ID, c.Dimension, res.Passed, res.Runs-res.Errors, res.Cost))
		}
		if ctx.Err() != nil {
			break
		}
	}

	out.Duration = time.Since(started)
	return out, nil
}

func (r *Runner) runCase(
	ctx context.Context, s *Suite, c Case,
	ep *domain.ModelEndpoint, adapter provider.Adapter, cred provider.Credential,
) CaseResult {
	res := CaseResult{CaseID: c.ID, Dimension: c.Dimension, Weight: c.Weight}
	started := time.Now()

	for range s.Repeats {
		res.Runs++

		req := c.Request(s.Temperature)
		callCtx := ctx
		if r.Timeout > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, r.Timeout)
			defer cancel()
		}

		resp, err := adapter.Chat(callCtx, req, ep, cred)
		if err != nil {
			res.Errors++
			res.Failures = append(res.Failures, "call failed: "+err.Error())
			continue
		}
		res.Cost += resp.Usage.Cost(ep)

		g := grade(c, req, resp)
		if g.Passed {
			res.Passed++
			continue
		}
		res.Failures = append(res.Failures, g.Failures...)
	}

	res.Duration = time.Since(started)
	res.Failures = dedupe(res.Failures)
	return res
}

// Report renders results as a human-readable summary plus the catalog fragment.
func Report(w io.Writer, results []EndpointResult, verifiedOn time.Time, source string) {
	for _, r := range results {
		fmt.Fprintf(w, "\n%s  (%s)\n", r.Endpoint, r.Model)
		fmt.Fprintf(w, "  %d cases, %s, %s", len(r.Cases), r.Duration.Round(time.Millisecond), r.Cost)
		if n := r.Errored(); n > 0 {
			fmt.Fprintf(w, ", %d errored run(s) excluded", n)
		}
		fmt.Fprintln(w)

		for _, c := range r.Cases {
			status := "pass"
			if c.PassRate() < 1 {
				status = "FAIL"
			}
			fmt.Fprintf(w, "    %-4s %-28s %-14s %.0f%%\n",
				status, c.CaseID, c.Dimension, c.PassRate()*100)
			for _, f := range c.Failures {
				fmt.Fprintf(w, "           %s\n", f)
			}
		}
	}

	fmt.Fprintln(w, "\n# Catalog fragment. Paste under the endpoint's `quality:` key.")
	fmt.Fprintln(w, "# These are measured pass rates, not asserted scores — which is the")
	fmt.Fprintln(w, "# distinction ADR-0009 exists to make. Re-run whenever the catalog changes.")
	for _, r := range results {
		scores := r.Scores()
		if len(scores) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n# %s\n", r.Endpoint)
		fmt.Fprintln(w, "    quality:")
		for _, dim := range domain.SortedKeys(scores) {
			fmt.Fprintf(w, "      %s: %.2f\n", dim, scores[dim])
		}
		fmt.Fprintf(w, "      # measured %s", verifiedOn.Format("2006-01-02"))
		if source != "" {
			fmt.Fprintf(w, " by %s", source)
		}
		fmt.Fprintln(w)
	}
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
