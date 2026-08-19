// Command relay-eval measures endpoint quality offline and emits the catalog
// fragment to record it.
//
// A separate binary rather than a subcommand of the gateway, because it is a
// different kind of program: it costs real money at real providers, it runs
// when a human decides to, and it must never be reachable from a request. The
// gateway has no code path that could invoke it.
//
//	relay-eval -suite config/eval/coding.yaml -endpoints openai/gpt-4o-mini@us-east
//
// The output is a `quality:` block to paste into the catalog. That paste is
// deliberately manual: a score that rewrote the catalog automatically would let
// one bad afternoon at a provider silently change how every request routes, and
// the catalog is the one file where a change should be reviewed like code.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/eval"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/anthropic"
	"github.com/Shashank-Panda/relay/internal/provider/ollama"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "relay-eval:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		suitePath   = flag.String("suite", "", "path to the eval suite (required)")
		catalogPath = flag.String("catalog", "config/catalog.yaml", "catalog to read endpoints from")
		endpoints   = flag.String("endpoints", "", "comma-separated endpoint IDs; empty means every priced endpoint")
		repeats     = flag.Int("repeats", 0, "override the suite's repeat count")
		timeout     = flag.Duration("timeout", 90*time.Second, "per-case timeout")
		source      = flag.String("source", "", "who ran this, recorded in the emitted attestation")
		dryRun      = flag.Bool("dry-run", false, "list what would be called, and call nothing")
	)
	flag.Parse()

	if *suitePath == "" {
		flag.Usage()
		return errors.New("-suite is required")
	}

	suite, err := eval.LoadFile(*suitePath)
	if err != nil {
		return err
	}
	if *repeats > 0 {
		suite.Repeats = *repeats
	}

	// Freshness is not checked. An eval is measuring model behaviour, and a
	// stale price does not change what a model answers — refusing to run
	// because somebody has not re-verified a price would block the exact task
	// that produces better catalog data.
	cat, err := catalog.LoadFile(*catalogPath, catalog.Options{})
	if err != nil {
		return err
	}

	targets, err := selectEndpoints(cat, *endpoints)
	if err != nil {
		return err
	}

	fmt.Printf("suite %q: %d cases x %d repeat(s) against %d endpoint(s)\n",
		suite.Name, len(suite.Cases), suite.Repeats, len(targets))

	if *dryRun {
		for _, ep := range targets {
			fmt.Printf("  would call %s (%s/%s), %d requests\n",
				ep.ID, ep.Provider, ep.Model, len(suite.Cases)*suite.Repeats)
		}
		return nil
	}

	client := provider.NewClient(provider.DefaultClientOptions())
	registry := provider.NewRegistry(
		ollama.New(client), openai.New(client), anthropic.New(client),
	)
	runner := &eval.Runner{
		Registry: registry,
		Resolver: &provider.EnvResolver{Free: map[string]bool{"local": true}},
		Timeout:  *timeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var results []eval.EndpointResult
	for _, ep := range targets {
		fmt.Printf("\n%s\n", ep.ID)
		runner.Progress = func(line string) { fmt.Println("  " + line) }

		res, err := runner.Run(ctx, suite, ep)
		if err != nil {
			// One unreachable endpoint must not discard the results for the
			// others, which have already been paid for.
			fmt.Fprintf(os.Stderr, "  skipped: %v\n", err)
			continue
		}
		results = append(results, res)

		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\ninterrupted; reporting what completed")
			break
		}
	}

	if len(results) == 0 {
		return errors.New("no endpoint produced results")
	}
	eval.Report(os.Stdout, results, time.Now().UTC(), *source)
	return nil
}

// selectEndpoints resolves the -endpoints flag against the catalog.
func selectEndpoints(cat *domain.Catalog, list string) ([]*domain.ModelEndpoint, error) {
	if list != "" {
		var out []*domain.ModelEndpoint
		for _, id := range strings.Split(list, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			ep, ok := cat.Endpoint(id)
			if !ok {
				return nil, fmt.Errorf("endpoint %q is not in catalog %s", id, cat.Version)
			}
			out = append(out, ep)
		}
		if len(out) == 0 {
			return nil, errors.New("-endpoints listed nothing")
		}
		return out, nil
	}

	var out []*domain.ModelEndpoint
	for _, id := range domain.SortedKeys(cat.Endpoints) {
		ep := cat.Endpoints[id]
		if ep.Lifecycle.Status == domain.StatusRetired {
			// Scoring an endpoint nothing may route to is a bill for a number
			// that cannot be used.
			continue
		}
		out = append(out, ep)
	}
	if len(out) == 0 {
		return nil, errors.New("catalog has no eligible endpoints")
	}
	return out, nil
}
