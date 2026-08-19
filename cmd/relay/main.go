// Command relay runs the Relay gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/Shashank-Panda/relay/internal/admit"
	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/health"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/metrics"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/anthropic"
	"github.com/Shashank-Panda/relay/internal/provider/ollama"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/respcache"
	"github.com/Shashank-Panda/relay/internal/server"
	"github.com/Shashank-Panda/relay/internal/tenant"
)

type config struct {
	addr           string
	adminAddr      string
	catalogPath    string
	tenantsPath    string
	ledgerPath     string
	cacheEntries   int
	cacheMB        int
	maxPriceAge    time.Duration
	maxCatalogAge  time.Duration
	maxInFlight    int
	maxStreams     int
	maxPerEndpoint int
	totalDeadline  time.Duration
	attemptTimeout time.Duration
	maxAttempts    int
	logLevel       string
	logFormat      string
	drainTimeout   time.Duration
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", ":8080", "listen address for the data plane")
	// Loopback by default. The savings query exposes every tenant's spend and
	// there is no authentication on it until Phase 7, so the bind address is
	// the control.
	flag.StringVar(&cfg.adminAddr, "admin-addr", "127.0.0.1:9090",
		"listen address for /metrics and /savings; empty disables it")
	flag.StringVar(&cfg.catalogPath, "catalog", "config/catalog.yaml", "path to the model catalog")
	flag.StringVar(&cfg.tenantsPath, "tenants", "config/tenants.yaml",
		"path to the tenant registry; a missing file means every request is anonymous")
	flag.StringVar(&cfg.ledgerPath, "ledger", "data/ledger.jsonl",
		"append-only savings ledger; empty disables durable metering")
	flag.IntVar(&cfg.cacheEntries, "cache-entries", respcache.DefaultMaxEntries,
		"exact-match response cache size in entries; 0 or less disables the cache")
	flag.IntVar(&cfg.cacheMB, "cache-mb", respcache.DefaultMaxBytes>>20,
		"exact-match response cache size in MiB")
	flag.DurationVar(&cfg.maxPriceAge, "max-price-age", 90*24*time.Hour,
		"reject pricing not verified within this window; 0 disables the check")
	flag.DurationVar(&cfg.maxCatalogAge, "max-catalog-age", 24*time.Hour,
		"beyond this, degrade to baseline passthrough rather than route on stale prices; 0 disables")
	flag.IntVar(&cfg.maxInFlight, "max-in-flight", admit.DefaultConfig().MaxInFlight,
		"concurrent provider calls before shedding; 0 disables admission control")
	flag.IntVar(&cfg.maxStreams, "max-streams", admit.DefaultConfig().MaxStreams,
		"concurrent streaming responses, counted within -max-in-flight")
	flag.IntVar(&cfg.maxPerEndpoint, "max-per-endpoint", admit.DefaultConfig().MaxPerEndpoint,
		"concurrent calls to any one endpoint, so a slow provider cannot starve the healthy ones")
	flag.IntVar(&cfg.maxAttempts, "max-attempts", execute.DefaultPolicy().MaxAttempts,
		"total provider calls per request, across retries and failover; every one is a real charge")
	flag.DurationVar(&cfg.attemptTimeout, "attempt-timeout", execute.DefaultPolicy().AttemptTimeout,
		"bounds one call; for streams it bounds opening the stream, never the stream itself")
	flag.DurationVar(&cfg.totalDeadline, "request-deadline", execute.DefaultPolicy().TotalDeadline,
		"bounds attempts and backoff for one request")
	flag.StringVar(&cfg.logLevel, "log-level", "info", "debug | info | warn | error")
	flag.StringVar(&cfg.logFormat, "log-format", "json", "json | text")
	flag.DurationVar(&cfg.drainTimeout, "drain-timeout", 30*time.Second,
		"how long in-flight requests may finish after shutdown begins")
	flag.Parse()

	log := newLogger(cfg.logLevel, cfg.logFormat)

	// The catalog is loaded and validated before anything starts listening. A
	// process that came up with a broken catalog would report healthy and fail
	// every request, which is strictly worse than failing to start.
	cat, err := catalog.LoadFile(cfg.catalogPath, catalog.Options{MaxPriceAge: cfg.maxPriceAge})
	if err != nil {
		return err
	}
	store := catalog.NewStore(cat).WithMaxAge(cfg.maxCatalogAge)

	client := provider.NewClient(provider.DefaultClientOptions())
	registry := provider.NewRegistry(
		ollama.New(client),
		openai.New(client),
		anthropic.New(client),
	)

	// A local Ollama needs no secret. Listing it as free is what keeps a
	// dummy key out of the environment — and a dummy key is indistinguishable
	// from a real one that stopped working.
	resolver := &provider.EnvResolver{Free: map[string]bool{"local": true}}

	if unreachable := registry.Unreachable(cat); len(unreachable) > 0 {
		// Not fatal: the other endpoints still work, and refusing to start over
		// one unrouteable entry would make adding a provider to the catalog a
		// deployment risk.
		log.Warn("catalog names providers this build cannot reach",
			"endpoints", unreachable)
	}

	tenants, err := tenant.LoadFile(cfg.tenantsPath)
	if err != nil {
		return err
	}

	// Metrics and the ledger are fed from the same Record, so a dashboard and a
	// savings report cannot disagree about the product's headline number.
	promReg := prometheus.NewRegistry()
	promReg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	mx := metrics.New(promReg)

	agg := meter.NewAggregator(time.Now().UTC())

	// RouteStats is a sink rather than a separate instrumentation point for the
	// same reason Metrics is: the output-ceiling lever, the savings report, and
	// the dashboards must all be describing the same requests, and the way to
	// guarantee that is for all three to read the same Record.
	stats := meter.NewRouteStats(meter.DefaultMinSamples)
	sinks := []meter.Sink{agg, mx, stats}

	if cfg.ledgerPath != "" {
		ledger, err := meter.OpenFile(cfg.ledgerPath, 64)
		if err != nil {
			return err
		}
		defer ledger.Close()
		sinks = append(sinks, ledger)
	}
	mtr := meter.New(meter.DefaultBuffer, sinks...)

	// Off entirely when sized to nothing, and off by route otherwise. Two gates,
	// because "no route opted in" and "the operator disabled it" are different
	// decisions made by different people.
	var cache *respcache.Store
	if cfg.cacheEntries > 0 {
		cache = respcache.New(respcache.Options{
			MaxEntries: cfg.cacheEntries,
			MaxBytes:   cfg.cacheMB << 20,
		})
	}

	// The degradation counter is not optional. Passthrough is silent by design —
	// the customer sees a normal response — so without this Relay can be
	// optimizing nothing, saving nothing, and still look healthy on every other
	// dashboard (ADR-0010). Alert on it.
	degraded := func(component, reason string) { mx.ObserveDegraded(component, reason) }

	tracker := health.New(health.DefaultConfig())

	limiter := admit.New(admit.Config{
		MaxInFlight:    cfg.maxInFlight,
		MaxStreams:     cfg.maxStreams,
		MaxPerEndpoint: cfg.maxPerEndpoint,
	})

	executor := execute.New(registry, resolver).
		WithTracker(tracker).
		WithDegradedHook(degraded).
		WithAttemptHook(func(a execute.Attempt) {
			mx.ObserveAttempt(a.EndpointID, a.Retry, string(a.Class))
		}).
		WithPolicy(execute.Policy{
			MaxAttempts:    cfg.maxAttempts,
			AttemptTimeout: cfg.attemptTimeout,
			TotalDeadline:  cfg.totalDeadline,
		})

	gw := &gateway.Gateway{
		Store:      store,
		Executor:   executor,
		Tenants:    tenants,
		Stats:      stats,
		Cache:      cache,
		Health:     tracker,
		OnDegraded: degraded,
		// The fallback when no tenant registry is configured. Strict: nobody
		// gets substituted by leaving a file absent.
		Policy: domain.DefaultPolicy(),
	}

	srv := server.New(gw, store, registry, server.Options{
		Logger:  log,
		Tenants: tenants,
		Meter:   mtr,
		Metrics: mx,
		Admit:   limiter,
	})

	httpSrv := &http.Server{
		Addr:    cfg.addr,
		Handler: srv.Handler(),

		// ReadHeaderTimeout bounds a slowloris. Deliberately no WriteTimeout:
		// it would bound the whole response and so would terminate every
		// long-running stream mid-generation. Streams are bounded by the
		// request context instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var adminSrv *http.Server
	if cfg.adminAddr != "" {
		adminSrv = &http.Server{
			Addr: cfg.adminAddr,
			Handler: server.NewAdmin(agg, mtr, promReg).
				WithOptimizer(cache, stats).
				WithReliability(tracker, limiter, store).
				Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Info("admin listening", "addr", cfg.adminAddr, "paths", "/metrics /savings /health")
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// Not fatal. The control plane going down must not take
				// inference with it — that is the whole point of the split.
				log.Error("admin listener stopped", "error", err)
			}
		}()
	}

	// Publish the drop counter periodically. A rising value means the savings
	// ledger is incomplete, which is a correctness problem for the report
	// rather than merely a monitoring one.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mx.ObserveDropped(mtr.Dropped())
				// A gauge has to be pushed; nothing else observes the breakers
				// from outside a request, and "how many endpoints are currently
				// unavailable" is the first question during an incident.
				mx.ObserveBreakers(tracker.Open())
				mx.ObserveQualityPenalties(tracker.Penalties())
				if store.Stale() {
					degraded("catalog", "stale_snapshot")
				}
			}
		}
	}()

	errs := make(chan error, 1)
	go func() {
		log.Info("relay listening",
			"addr", cfg.addr,
			"catalog_version", cat.Version,
			"endpoints", len(cat.Endpoints),
			"routes", len(cat.Routes),
			"providers", registry.Providers(),
			"tenants", tenants.IDs(),
			"ledger", orNone(cfg.ledgerPath),
			"response_cache", cacheDescription(cache, cfg.cacheEntries, cfg.cacheMB),
			"max_in_flight", cfg.maxInFlight,
			"max_attempts", cfg.maxAttempts,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// Readiness flips first so load balancers stop sending traffic before the
	// socket closes. A process that reports healthy while draining collects
	// requests it has already decided not to finish.
	log.Info("shutdown starting", "drain_timeout", cfg.drainTimeout)
	srv.BeginShutdown()

	drain, cancel := context.WithTimeout(context.Background(), cfg.drainTimeout)
	defer cancel()

	if err := httpSrv.Shutdown(drain); err != nil {
		// Shutdown returns the deadline error when streams are still open. The
		// process exits anyway; saying so is more useful than a clean exit that
		// hides a truncated response.
		log.Warn("drain did not complete", "error", err)
	}

	// The meter closes *after* the HTTP server has drained, so the records for
	// in-flight requests are already queued and survive. Closing it first would
	// silently discard the last few seconds of the ledger on every deploy —
	// a small, systematic, invisible undercount.
	mx.ObserveDropped(mtr.Dropped())
	if err := mtr.Close(drain); err != nil {
		log.Warn("metering did not flush", "error", err)
	}
	if n := mtr.Dropped(); n > 0 {
		log.Warn("ledger records were dropped; the savings report is incomplete", "dropped", n)
	}

	if adminSrv != nil {
		_ = adminSrv.Shutdown(drain)
	}

	log.Info("shutdown complete", "records_written", mtr.Written())
	return nil
}

// cacheDescription renders the cache configuration for the startup line.
//
// Logged because a disabled response cache is indistinguishable at runtime from
// an enabled one that nothing has opted into, and the first thing anybody asks
// about a cache with no hits is which of the two it is.
func cacheDescription(c *respcache.Store, entries, mb int) string {
	if c == nil {
		return "(disabled)"
	}
	return fmt.Sprintf("%d entries / %d MiB, per-route opt-in", entries, mb)
}

func orNone(s string) string {
	if s == "" {
		return "(disabled)"
	}
	return s
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
