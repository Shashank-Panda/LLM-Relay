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

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/provider/anthropic"
	"github.com/Shashank-Panda/relay/internal/provider/ollama"
	"github.com/Shashank-Panda/relay/internal/provider/openai"
	"github.com/Shashank-Panda/relay/internal/server"
)

type config struct {
	addr         string
	catalogPath  string
	maxPriceAge  time.Duration
	logLevel     string
	logFormat    string
	drainTimeout time.Duration
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "relay:", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", ":8080", "listen address")
	flag.StringVar(&cfg.catalogPath, "catalog", "config/catalog.yaml", "path to the model catalog")
	flag.DurationVar(&cfg.maxPriceAge, "max-price-age", 90*24*time.Hour,
		"reject pricing not verified within this window; 0 disables the check")
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
	store := catalog.NewStore(cat)

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

	gw := &gateway.Gateway{
		Store:    store,
		Executor: execute.New(registry, resolver),
		// Phase 1 is strict for everybody: serve exactly what was asked, never
		// substitute. Phase 7 makes this per-tenant.
		Policy: domain.DefaultPolicy(),
	}

	srv := server.New(gw, store, registry, server.Options{Logger: log})

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

	errs := make(chan error, 1)
	go func() {
		log.Info("relay listening",
			"addr", cfg.addr,
			"catalog_version", cat.Version,
			"endpoints", len(cat.Endpoints),
			"routes", len(cat.Routes),
			"providers", registry.Providers(),
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

	log.Info("shutdown complete")
	return nil
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
