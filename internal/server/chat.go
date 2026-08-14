package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/execute"
	"github.com/Shashank-Panda/relay/internal/gateway"
	"github.com/Shashank-Panda/relay/internal/meter"
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

	// HeaderDecision is a compact summary of why this endpoint was chosen.
	// Bounded deliberately: the full Decision belongs in the ledger and in a
	// dry-run response, not in a header that proxies may truncate.
	HeaderDecision = "X-Relay-Decision"

	// HeaderSaved is what this request saved, in USD.
	//
	// On a non-streaming response it comes from provider-reported actuals. On a
	// stream it cannot: headers are fixed before the first byte and the token
	// counts arrive at the end. Streaming therefore carries the pre-flight
	// estimate and sets HeaderSavedEstimated so the two are never confused.
	HeaderSaved          = "X-Relay-Saved-Usd"
	HeaderSavedEstimated = "X-Relay-Saved-Estimated"

	// HeaderShadowSaved is what optimize mode would have saved. Present only in
	// shadow mode, and never merged with HeaderSaved: one is money saved, the
	// other is money that could have been.
	HeaderShadowSaved = "X-Relay-Shadow-Saved-Usd"
)

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
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

	tn := tenantOf(r.Context(), s.tenants)

	prepared, err := s.gw.Prepare(req, tn, body.Model, r.Header.Get(HeaderPin))
	if err != nil {
		s.writePrepareError(w, err)
		return
	}

	s.setDisclosureHeaders(w, prepared)

	if req.Stream {
		s.streamCompletion(w, r, prepared, start)
		return
	}
	s.chatCompletion(w, r, prepared, start)
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
	h.Set(HeaderDecision, decisionSummary(d))
}

// decisionSummary renders the ranking compactly enough for a header.
//
// Top three only, and no reasons. A header is a bounded medium — proxies vary,
// and an 8KB limit is common — so this is the summary that says *what* was
// decided; the ledger and the dry-run endpoint carry the full explanation.
func decisionSummary(d *domain.Decision) string {
	type entry struct {
		Endpoint string  `json:"endpoint"`
		Total    float64 `json:"total"`
	}
	out := struct {
		Route          string  `json:"route,omitempty"`
		Chosen         string  `json:"chosen"`
		Counterfactual string  `json:"counterfactual,omitempty"`
		Mode           string  `json:"mode"`
		Ranked         []entry `json:"ranked,omitempty"`
		Rejected       int     `json:"rejected,omitempty"`
		Fallback       bool    `json:"fallback,omitempty"`
	}{
		Route:          d.RouteName,
		Chosen:         d.Chosen,
		Counterfactual: d.Counterfactual,
		Mode:           string(d.Baseline.Mode),
		Rejected:       len(d.Rejected),
		Fallback:       d.UsedFallback,
	}
	for i, c := range d.Ranked {
		if i == 3 {
			break
		}
		out.Ranked = append(out.Ranked, entry{c.EndpointID, round3(c.Total)})
	}

	buf, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(buf)
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

// setSavingsHeaders reports the money figures once they are known.
func (s *Server) setSavingsHeaders(w http.ResponseWriter, rec *meter.Record, estimated bool) {
	if !rec.SavingMeasured {
		return
	}
	h := w.Header()
	h.Set(HeaderSaved, usd(rec.Saved))
	if estimated {
		// The caller must be able to tell a measured figure from a pre-flight
		// one. A dashboard that summed both would be summing guesses.
		h.Set(HeaderSavedEstimated, "true")
	}
	if rec.Counterfactual != "" {
		h.Set(HeaderShadowSaved, usd(rec.ShadowSaved))
	}
}

func usd(m domain.Money) string {
	return strconv.FormatFloat(m.Dollars(), 'f', 6, 64)
}

func (s *Server) chatCompletion(w http.ResponseWriter, r *http.Request, p *gateway.Prepared, start time.Time) {
	rec := p.NewRecord(false)
	rec.At = start

	providerStart := time.Now()
	resp, _, att, err := s.gw.Chat(r.Context(), p)
	rec.ProviderDuration = time.Since(providerStart)

	if err != nil {
		rec.Outcome = outcomeFor(err)
		rec.ErrorClass = string(provider.ClassOf(err))
		rec.Duration = time.Since(start)
		s.record(rec)

		s.logAttempt(r, p, att, nil)
		wire.WriteProviderError(w, err)
		return
	}

	p.Price(&rec, resp.Usage)
	rec.Outcome = meter.OutcomeSuccess
	rec.Duration = time.Since(start)

	// Non-streaming can report actuals, because nothing has been written yet.
	s.setSavingsHeaders(w, &rec, false)

	s.record(rec)
	s.logCompletion(r, p, rec)

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
func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, p *gateway.Prepared, start time.Time) {
	rec := p.NewRecord(true)
	rec.At = start

	// The estimate is all that can be known before the first byte, and the
	// header must be written now or not at all.
	s.setEstimatedSavings(w, p)

	providerStart := time.Now()
	stream, _, att, err := s.gw.Stream(r.Context(), p)
	if err != nil {
		rec.ProviderDuration = time.Since(providerStart)
		rec.Duration = time.Since(start)
		rec.Outcome = outcomeFor(err)
		rec.ErrorClass = string(provider.ClassOf(err))
		s.record(rec)

		s.logAttempt(r, p, att, nil)
		// Nothing has been written yet, so this is still a normal JSON error
		// with a real status code. After the first frame it could not be.
		wire.WriteProviderError(w, err)
		return
	}
	defer stream.Close()

	s.streamsOpened()
	defer s.streamsClosed()

	sw := wire.NewStreamWriter(w)
	id := "chatcmpl-" + p.Request.ID
	created := time.Now().Unix()

	finish := func(outcome meter.Outcome, err error) {
		rec.ProviderDuration = time.Since(providerStart)
		rec.Duration = time.Since(start)
		rec.Outcome = outcome
		if err != nil {
			rec.ErrorClass = string(provider.ClassOf(err))
		}
		s.record(rec)
	}

	// The opening frame carries only role:"assistant". SDKs that build a
	// message incrementally use it to initialise the object; without it the
	// first content delta is applied to nothing.
	if err := sw.Send(wire.RoleChunk(id, p.RequestedModel, created)); err != nil {
		finish(meter.OutcomeCancelled, err)
		s.logAttempt(r, p, att, nil)
		return
	}

	var firstToken time.Time
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			finish(outcomeFor(err), err)
			s.logAttempt(r, p, att, err)
			// Past the first byte the status code is already sent, so the only
			// honest way to report this is inside the stream. Closing the
			// connection instead is indistinguishable at the client from a
			// network fault, and a client that cannot tell them apart retries a
			// request guaranteed to fail again.
			sw.Error(providerMessage(err), wire.TypeAPIError, string(provider.ClassOf(err)))
			return
		}

		if firstToken.IsZero() && (chunk.Text != "" || chunk.ToolCall != nil) {
			firstToken = time.Now()
			rec.TTFT = firstToken.Sub(start)
		}

		if err := sw.Send(wire.EncodeChunk(chunk, id, p.RequestedModel, created)); err != nil {
			// The client is gone. Returning cancels r.Context(), which aborts
			// the upstream call and stops the meter.
			finish(meter.OutcomeCancelled, err)
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
			finish(meter.OutcomeCancelled, err)
			s.logDisconnect(r, p, att)
			return
		}
	}
	_ = sw.Done()

	// Priced from actuals even though the header could not be: the ledger is
	// what the savings report is built from, and it has no such constraint.
	p.Price(&rec, usage)
	finish(meter.OutcomeSuccess, nil)
	s.logCompletion(r, p, rec)
}

// setEstimatedSavings writes the pre-flight figure for a streaming response.
//
// Estimated because output length is unknowable before generation. Flagged as
// such, rather than omitted, because a caller watching a stream still wants a
// signal — and rather than presented as measured, because that would put a
// guess into a field a dashboard might sum.
func (s *Server) setEstimatedSavings(w http.ResponseWriter, p *gateway.Prepared) {
	d := p.Decision
	if !d.SavingMeasured {
		return
	}
	h := w.Header()
	h.Set(HeaderSaved, usd(d.EstimatedSaved))
	h.Set(HeaderSavedEstimated, "true")
	if shadow, ok := d.ShadowSaving(); ok {
		h.Set(HeaderShadowSaved, usd(shadow))
	}
}

func (s *Server) record(rec meter.Record) {
	if s.meter == nil {
		return
	}
	s.meter.Record(rec)
}

func (s *Server) streamsOpened() {
	if s.metrics != nil {
		s.metrics.StreamsActive.Inc()
	}
}

func (s *Server) streamsClosed() {
	if s.metrics != nil {
		s.metrics.StreamsActive.Dec()
	}
}

func outcomeFor(err error) meter.Outcome {
	if err == nil {
		return meter.OutcomeSuccess
	}
	if provider.ClassOf(err) == provider.ClassCancelled {
		// A client hanging up is not a failure of Relay's, and counting it as
		// one would put every cancelled stream into the error budget.
		return meter.OutcomeCancelled
	}
	return meter.OutcomeError
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
func (s *Server) logCompletion(r *http.Request, p *gateway.Prepared, rec meter.Record) {
	attrs := []slog.Attr{
		slog.String("request_id", rec.RequestID),
		slog.String("tenant", rec.Tenant),
		slog.String("model_requested", rec.RequestedModel),
		slog.String("endpoint", rec.Endpoint),
		slog.String("route", rec.RouteName),
		slog.String("mode", string(rec.Mode)),
		slog.String("catalog_version", rec.CatalogVersion),
		slog.Int("input_tokens", rec.InputTokens),
		slog.Int("cached_input_tokens", rec.CachedInputTokens),
		slog.Int("output_tokens", rec.OutputTokens),
		slog.Bool("usage_estimated", rec.UsageEstimated),
		slog.String("cost", rec.Cost.String()),
		slog.Bool("substituted", rec.Substituted),
		slog.Duration("duration", rec.Duration),
		slog.Duration("overhead", rec.Overhead()),
	}
	if rec.SavingMeasured {
		attrs = append(attrs,
			slog.String("baseline_cost", rec.BaselineCost.String()),
			slog.String("saved", rec.Saved.String()),
		)
		if rec.Counterfactual != "" {
			attrs = append(attrs,
				slog.String("counterfactual", rec.Counterfactual),
				slog.String("shadow_saved", rec.ShadowSaved.String()),
			)
		}
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
		slog.String("tenant", p.TenantID()),
		slog.String("endpoint", att.EndpointID),
		slog.String("error_class", string(class)),
		slog.String("error", errString(err)),
	)
}

func (s *Server) logDisconnect(r *http.Request, p *gateway.Prepared, att execute.Attempt) {
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "client disconnected mid-stream",
		slog.String("request_id", p.Request.ID),
		slog.String("tenant", p.TenantID()),
		slog.String("endpoint", att.EndpointID),
	)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
