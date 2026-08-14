package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Shashank-Panda/relay/internal/wire"
)

// handleHealthz answers whether the process is alive.
//
// Deliberately unconditional. Liveness decides whether to *restart* the
// process, and a liveness probe that fails because a provider is down turns a
// provider outage into a restart loop — which removes the one component that
// could still have served the request from its fallback.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz answers whether this instance should receive traffic.
//
// Not ready means: shutting down, or holding no usable catalog. Both are states
// where another replica would serve the request better, and neither is a reason
// to restart.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "shutting_down",
		})
		return
	}

	cat := s.store.Current()
	if cat == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "no_catalog",
			"detail": "no catalog snapshot has been loaded",
		})
		return
	}

	body := map[string]any{
		"status":          "ok",
		"catalog_version": cat.Version,
		"endpoints":       len(cat.Endpoints),
		"routes":          len(cat.Routes),
	}

	// An endpoint whose provider has no adapter is selectable by routing and
	// unreachable by execution. Surfaced here rather than discovered at request
	// time, because the operator who added it is the person best placed to fix
	// it and they are still watching the deploy.
	if unreachable := s.registry.Unreachable(cat); len(unreachable) > 0 {
		body["unreachable_endpoints"] = unreachable
	}

	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cat := s.store.Current()
	if cat == nil {
		wire.WriteError(w, http.StatusServiceUnavailable,
			"no catalog is loaded", wire.TypeAPIError, "")
		return
	}
	// A fixed creation timestamp. The field is required by the schema and has
	// no meaningful value here; emitting time.Now() would make two identical
	// requests return different bodies and defeat any client-side caching.
	writeJSON(w, http.StatusOK, wire.EncodeModels(cat, catalogEpoch))
}

// catalogEpoch is an arbitrary stable timestamp for the models list.
var catalogEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
