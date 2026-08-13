// Package server is the HTTP surface: routing, middleware, and the handlers
// that turn bytes into a pipeline call and back.
//
// It holds no routing or provider logic. Everything it does is translate.
package server

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// Options configures the server.
type Options struct {
	// MaxBodyBytes caps the request body. Enforced before parsing, not after:
	// a limit applied to an already-decoded document has already paid the
	// memory cost it exists to prevent.
	MaxBodyBytes int64

	// StreamHeartbeat is how often a comment frame is sent while waiting for
	// the provider's first token, to stop intermediaries closing the
	// connection. Zero disables it.
	StreamHeartbeat time.Duration

	Logger *slog.Logger
}

func DefaultOptions() Options {
	return Options{
		MaxBodyBytes:    10 << 20, // 10 MiB — a large multimodal request, not an attack
		StreamHeartbeat: 15 * time.Second,
		Logger:          slog.Default(),
	}
}

type Server struct {
	gw       *gateway.Gateway
	store    *catalog.Store
	registry *provider.Registry
	opts     Options
	log      *slog.Logger

	// ready flips to false the instant shutdown begins, so load balancers stop
	// sending traffic before the socket closes. A process that reports healthy
	// while draining collects requests it has already decided not to finish.
	ready atomic.Bool
}

func New(gw *gateway.Gateway, store *catalog.Store, registry *provider.Registry, opts Options) *Server {
	d := DefaultOptions()
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = d.MaxBodyBytes
	}
	if opts.Logger == nil {
		opts.Logger = d.Logger
	}

	s := &Server{gw: gw, store: store, registry: registry, opts: opts, log: opts.Logger}
	s.ready.Store(true)
	return s
}

// Handler builds the mux.
//
// Method-qualified patterns (Go 1.22) mean a GET to a POST-only path returns
// 405 rather than being handled, without a per-handler method check that
// somebody eventually forgets to write.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	return chain(mux,
		wrapWriter,
		withRequestID,
		withRecovery(s.log),
		withAccessLog(s.log),
	)
}

// BeginShutdown marks the instance not-ready. Called before the HTTP server
// starts draining so that load balancers have time to notice.
func (s *Server) BeginShutdown() { s.ready.Store(false) }

// Ready reports readiness, for tests and for the handler.
func (s *Server) Ready() bool { return s.ready.Load() }
