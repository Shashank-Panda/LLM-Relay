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

	"github.com/Shashank-Panda/relay/internal/admit"
	"github.com/Shashank-Panda/relay/internal/catalog"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/meter"
	"github.com/Shashank-Panda/relay/internal/metrics"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/tenant"
)

// Options configures the server.
type Options struct {
	// MaxBodyBytes caps the request body. Enforced before parsing, not after:
	// a limit applied to an already-decoded document has already paid the
	// memory cost it exists to prevent.
	MaxBodyBytes int64

	// StreamHeartbeat is how often an SSE comment frame is sent while a stream
	// is open but producing nothing, to stop intermediaries closing a
	// connection that is merely waiting on a slow model.
	//
	// Zero means unset and takes DefaultOptions' value; a negative duration
	// disables it. That asymmetry is deliberate — the default has to be on,
	// because a reasoning model thinking for ninety seconds is indistinguishable
	// from a dead connection to every proxy in the path, and the failure looks
	// like a truncated answer rather than like a timeout.
	StreamHeartbeat time.Duration

	Logger *slog.Logger

	// Tenants resolves API keys. Nil means every request is anonymous and runs
	// under the default policy, which is Phase 1's behaviour.
	Tenants *tenant.Registry

	// Meter receives one ledger record per request. Nil disables metering
	// entirely — the request path is unaffected either way, because recording
	// never blocks.
	Meter *meter.Meter

	// Metrics is the Prometheus instrumentation. Nil disables it.
	Metrics *metrics.Metrics

	// AllowInsecureCredentials permits X-Relay-Credential over plaintext HTTP.
	//
	// Off by default: a provider key sent in the clear is a compromised key, and
	// the request carrying it succeeds, so nothing else in the system would ever
	// report that it happened. On for a local deployment, where the alternative
	// is asking someone to terminate TLS to try a demo.
	AllowInsecureCredentials bool

	// Admit bounds concurrent work. Nil admits everything, which is Phase 1's
	// behaviour: correct for a single-tenant test deployment and not something
	// to run in front of production traffic.
	Admit *admit.Limiter
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

	// tenants and meter may both be nil, in which case Phase 1's behaviour
	// applies: every request anonymous, nothing recorded.
	tenants *tenant.Registry
	meter   *meter.Meter
	metrics *metrics.Metrics
	admit   *admit.Limiter

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
	// Zero means "unset", not "disabled". A caller that genuinely wants no
	// heartbeat passes a negative duration, because defaulting a zero to off
	// would make an unconfigured server silently drop long streams behind any
	// proxy with an idle timeout — which is every proxy.
	if opts.StreamHeartbeat == 0 {
		opts.StreamHeartbeat = d.StreamHeartbeat
	}

	s := &Server{
		gw: gw, store: store, registry: registry,
		opts: opts, log: opts.Logger,
		tenants: opts.Tenants,
		meter:   opts.Meter,
		metrics: opts.Metrics,
		admit:   opts.Admit,
	}
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
		// Authentication runs innermost, after recovery and logging, so a
		// rejected request still gets a request ID and an access-log line. An
		// auth failure nobody can find in the logs is an auth failure nobody
		// can debug.
		withTenant(s.tenants),
	)
}

// BeginShutdown marks the instance not-ready. Called before the HTTP server
// starts draining so that load balancers have time to notice.
func (s *Server) BeginShutdown() { s.ready.Store(false) }

// Ready reports readiness, for tests and for the handler.
func (s *Server) Ready() bool { return s.ready.Load() }
