package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/provider"
	"github.com/Shashank-Panda/relay/internal/routing"
	"github.com/Shashank-Panda/relay/internal/wire"
)

// Disclosure headers.
//
// Substitution is only acceptable because it is visible. These make it visible
// on every affected response rather than in a dashboard the caller has to go
// looking for — see ADR-0007.
const (
	HeaderPin         = "X-Relay-Pin"
	HeaderEndpoint    = "X-Relay-Endpoint"
	HeaderBaseline    = "X-Relay-Baseline"
	HeaderMode        = "X-Relay-Mode"
	HeaderSubstituted = "X-Relay-Substituted"
	HeaderCatalog     = "X-Relay-Catalog-Version"
)

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)

	var body wire.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			wire.WriteError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the maximum size", wire.TypeInvalidRequest, "")
			return
		}
		wire.WriteError(w, http.StatusBadRequest,
			"request body is not valid JSON: "+err.Error(), wire.TypeInvalidRequest, "")
		return
	}

	req, err := wire.Decode(&body)
	if err != nil {
		wire.WriteDecodeError(w, err)
		return
	}
	req.ID = RequestID(r.Context())

	prepared, err := s.gw.Prepare(req, body.Model, r.Header.Get(HeaderPin))
	if err != nil {
		s.writePrepareError(w, err)
		return
	}

	s.setDisclosureHeaders(w, prepared)

	if req.Stream {
		s.streamCompletion(w, r, prepared)
		return
	}
	s.chatCompletion(w, r, prepared)
}

// writePrepareError reports a failure that happened before any provider was
// contacted.
func (s *Server) writePrepareError(w http.ResponseWriter, err error) {
	var unknown *gateway.ErrUnknownModel
	if errors.As(err, &unknown) {
		body := wire.ErrorResponse{Error: wire.ErrorBody{
			Message: unknown.Error(),
			Type:    wire.TypeInvalidRequest,
			Code:    "model_not_found",
			Param:   "model",
		}}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(body)
		return
	}

	// No viable candidate and nothing to fall back to. A configuration problem
	// rather than a caller problem, so it reports as 503: retrying later may
	// well work, retrying the request unchanged right now will not.
	if errors.Is(err, routing.ErrNoCandidate) {
		wire.WriteError(w, http.StatusServiceUnavailable,
			"no endpoint is currently able to serve this request",
			wire.TypeAPIError, string(provider.ClassRetryOther))
		return
	}

	wire.WriteError(w, http.StatusInternalServerError,
		err.Error(), wire.TypeAPIError, "")
}

// setDisclosureHeaders must run before any body is written.
//
// After the first byte of a stream the headers are already on the wire, which
// is why Prepare is separate from execution: the decision has to exist before
// the response starts, or substitution could not be disclosed on the requests
// where it matters most.
func (s *Server) setDisclosureHeaders(w http.ResponseWriter, p *gateway.Prepared) {
	d := p.Decision
	h := w.Header()

	h.Set(HeaderEndpoint, d.Chosen)
	h.Set(HeaderCatalog, d.CatalogVersion)
	if d.Baseline.EndpointID != "" {
		h.Set(HeaderBaseline, d.Baseline.EndpointID)
	}
	if d.Baseline.Mode != "" {
		h.Set(HeaderMode, string(d.Baseline.Mode))
	}
	if d.Substituted() {
		// Mandatory, not optional, whenever the served endpoint differs from
		// what the caller named.
		h.Set(HeaderSubstituted, "true")
	}
}

func (s *Server) chatCompletion(w http.ResponseWriter, r *http.Request, p *gateway.Prepared) {
	resp, served, att, err := s.gw.Chat(r.Context(), p)
	if err != nil {
		s.logAttempt(r, p, att, nil)
		wire.WriteProviderError(w, err)
		return
	}

	cost, baseline, measured := p.Cost(resp.Usage, served)
	s.logCompletion(r, p, att, resp.Usage, cost, baseline, measured)

	writeJSON(w, http.StatusOK, wire.EncodeResponse(resp, p.RequestedModel, time.Now().Unix()))
}

// streamCompletion relays a provider stream as SSE.
//
// Three properties this function exists to guarantee:
//
//   - the stream is closed on every path, including the panic the recovery
//     middleware catches, because a leaked stream reader holds a connection and
//     a goroutine until the process dies;
//   - no goroutine is spawned per stream, so there is nothing that can outlive
//     the request context;
//   - a client disconnect propagates upstream, because r.Context() is what the
//     provider call was built from.
func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, p *gateway.Prepared) {
	stream, served, att, err := s.gw.Stream(r.Context(), p)
	if err != nil {
		s.logAttempt(r, p, att, nil)
		// Nothing has been written yet, so this is still a normal JSON error
		// with a real status code. After the first frame it could not be.
		wire.WriteProviderError(w, err)
		return
	}
	defer stream.Close()

	sw := wire.NewStreamWriter(w)
	id := "chatcmpl-" + p.Request.ID
	created := time.Now().Unix()

	// The opening frame carries only role:"assistant". SDKs that build a
	// message incrementally use it to initialise the object; without it the
	// first content delta is applied to nothing.
	if err := sw.Send(wire.RoleChunk(id, p.RequestedModel, created)); err != nil {
		s.logAttempt(r, p, att, nil)
		return
	}

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.logAttempt(r, p, att, err)
			// Past the first byte the status code is already sent, so the only
			// honest way to report this is inside the stream. Closing the
			// connection instead is indistinguishable at the client from a
			// network fault, and a client that cannot tell them apart retries a
			// request guaranteed to fail again.
			sw.Error(providerMessage(err), wire.TypeAPIError, string(provider.ClassOf(err)))
			return
		}

		if err := sw.Send(wire.EncodeChunk(chunk, id, p.RequestedModel, created)); err != nil {
			// The client is gone. Returning cancels r.Context(), which aborts
			// the upstream call and stops the meter.
			s.logDisconnect(r, p, att)
			return
		}
	}

	usage := provider.Usage{}
	if u := stream.Usage(); u != nil {
		usage = *u
	}

	if !usage.Estimated {
		if err := sw.Send(wire.UsageChunk(usage, id, p.RequestedModel, created)); err != nil {
			s.logDisconnect(r, p, att)
			return
		}
	}
	_ = sw.Done()

	cost, baseline, measured := p.Cost(usage, served)
	s.logCompletion(r, p, att, usage, cost, baseline, measured)
}

// providerMessage extracts a caller-safe description.
//
// provider.Error carries the credential reference and never the secret, so this
// cannot leak one — the guarantee lives in the type, not in this function's
// discipline.
func providerMessage(err error) string {
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Message != "" {
		return pe.Message
	}
	return "upstream provider error"
}

// logCompletion records one finished request.
//
// Token counts and cost; never prompt or response content. Architecture §1
// makes that a product commitment rather than a configuration flag someone
// might forget to set, and the way to keep a commitment like that is to have no
// code path that could violate it.
func (s *Server) logCompletion(
	r *http.Request, p *gateway.Prepared, att execute.Attempt,
	u provider.Usage, cost, baseline domain.Money, measured bool,
) {
	attrs := []slog.Attr{
		slog.String("request_id", p.Request.ID),
		slog.String("model_requested", p.RequestedModel),
		slog.String("endpoint", att.EndpointID),
		slog.String("route", p.Decision.RouteName),
		slog.String("catalog_version", p.Decision.CatalogVersion),
		slog.Int("input_tokens", u.InputTokens),
		slog.Int("cached_input_tokens", u.CachedInputTokens),
		slog.Int("output_tokens", u.OutputTokens),
		slog.Bool("usage_estimated", u.Estimated),
		slog.String("cost", cost.String()),
		slog.Bool("substituted", p.Decision.Substituted()),
	}
	if measured {
		attrs = append(attrs,
			slog.String("baseline_cost", baseline.String()),
			slog.String("saved", (baseline-cost).String()),
		)
	} else {
		// Unmeasured is not zero. Recording it as zero would let it be summed
		// with real measurements and quietly dilute every savings report that
		// contained one.
		attrs = append(attrs, slog.Bool("saving_measured", false))
	}

	s.log.LogAttrs(r.Context(), slog.LevelInfo, "completion", attrs...)
}

func (s *Server) logAttempt(r *http.Request, p *gateway.Prepared, att execute.Attempt, err error) {
	if err == nil {
		err = att.Err
	}
	class := att.Class
	if err != nil {
		class = provider.ClassOf(err)
	}

	level := slog.LevelWarn
	if class == provider.ClassCancelled {
		level = slog.LevelInfo
	}

	s.log.LogAttrs(r.Context(), level, "attempt failed",
		slog.String("request_id", p.Request.ID),
		slog.String("endpoint", att.EndpointID),
		slog.String("error_class", string(class)),
		slog.String("error", errString(err)),
	)
}

func (s *Server) logDisconnect(r *http.Request, p *gateway.Prepared, att execute.Attempt) {
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "client disconnected mid-stream",
		slog.String("request_id", p.Request.ID),
		slog.String("endpoint", att.EndpointID),
	)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
